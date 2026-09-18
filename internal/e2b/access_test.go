package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	process "github.com/aphronio/dorf/internal/e2b/gen/process"
	"github.com/aphronio/dorf/internal/e2b/gen/process/processconnect"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type accessProcess struct {
	processconnect.UnimplementedProcessHandler
	starts  int
	fail    bool
	started chan struct{}
	stopped chan struct{}
}

func (p *accessProcess) Start(ctx context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	p.starts++
	if p.started != nil {
		close(p.started)
		<-ctx.Done()
		close(p.stopped)
		return ctx.Err()
	}
	if err := stream.Send(startEvent(1)); err != nil {
		return err
	}
	if p.fail {
		return connect.NewError(connect.CodeUnavailable, errors.New("accepted process lost acknowledgement"))
	}
	return stream.Send(endEvent(0, true, "exited", ""))
}

type accessFixture struct {
	adapter                         Adapter
	owner                           provider.Ownership
	process                         *accessProcess
	finds, connects                 int
	foreign, missing, rejectConnect bool
}

func newAccessFixture(t testing.TB) *accessFixture {
	t.Helper()
	f := &accessFixture{owner: provider.Ownership{SessionID: "job-access", SandboxID: "sandbox-access", OwnershipNonce: strings.Repeat("a", 64)}, process: &accessProcess{}}
	_, processHandler := processconnect.NewProcessHandler(f.process)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/process.Process/") {
			processHandler.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/sandboxes":
			f.finds++
			metadata := e2bOwnership(f.owner).metadata()
			if f.foreign {
				metadata["dorf.ownership_nonce"] = "foreign"
			}
			items := []listedSandbox{}
			if !f.missing {
				items = append(items, listedSandbox{SandboxID: "provider-access", State: "paused", Metadata: metadata})
			}
			if err := json.NewEncoder(w).Encode(items); err != nil {
				t.Fatal(err)
			}
		case "/sandboxes/provider-access/connect":
			f.connects++
			if f.rejectConnect {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if err := json.NewEncoder(w).Encode(connectionResponse{SandboxID: "provider-access", Domain: "e2b.app", EnvdVersion: "0.6.2", EnvdAccessToken: "test-envd", TrafficAccessToken: "test-traffic"}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected access request %s", r.URL.Path)
		}
	})
	f.adapter = Adapter{Client: Client{APIURL: "https://e2b.test", APIKey: "test-key", HTTPClient: &http.Client{Transport: handlerTransport{handler: handler}}}, Config: AdapterConfig{Workspace: "/workspace/job", SandboxTimeout: 10 * time.Minute}}
	return f
}

func accessSequence(ctx context.Context, owner provider.Ownership, s provider.Sandbox) error {
	if _, err := s.Endpoint(ctx, owner, 4500); err != nil {
		return err
	}
	for range 2 {
		if _, err := s.Exec(ctx, owner, nil, "true"); err != nil {
			return err
		}
	}
	return nil
}

func TestScopedAccessCountsAndFreshCallbacks(t *testing.T) {
	f := newAccessFixture(t)
	if err := accessSequence(context.Background(), f.owner, f.adapter); err != nil {
		t.Fatal(err)
	}
	if f.finds != 3 || f.connects != 3 || f.process.starts != 2 {
		t.Fatalf("ordinary counts: lookup=%d connect=%d exec=%d", f.finds, f.connects, f.process.starts)
	}
	for range 2 {
		beforeFinds, beforeConnects := f.finds, f.connects
		if err := f.adapter.WithAccess(context.Background(), f.owner, func(s provider.Sandbox) error {
			if _, nested := s.(provider.ScopedAccess); nested {
				t.Fatal("bound view must not open nested scopes")
			}
			if _, batched := s.(provider.FileBatchReader); !batched {
				t.Fatal("bound view lost batched file capability")
			}
			return accessSequence(context.Background(), f.owner, s)
		}); err != nil {
			t.Fatal(err)
		}
		if f.finds-beforeFinds != 1 || f.connects-beforeConnects != 1 {
			t.Fatal("each callback must freshly resolve once")
		}
	}
	t.Log("Endpoint + 2 Exec: ordinary lookup/connect/exec=3/3/2; scoped=1/1/2; second callback freshly resolves")
}

func TestScopedAccessRejectsForeignCancelledAndExpiredUse(t *testing.T) {
	f := newAccessFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	var retained provider.Sandbox
	if err := f.adapter.WithAccess(ctx, f.owner, func(s provider.Sandbox) error {
		retained = s
		foreign := f.owner
		foreign.OwnershipNonce = "foreign"
		if _, err := s.Exec(ctx, foreign, nil, "true"); err == nil {
			t.Fatal("foreign scope reuse accepted")
		}
		cancel()
		if _, err := s.Exec(context.Background(), f.owner, nil, "true"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled scope: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.Endpoint(context.Background(), f.owner, 4500); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired scope: %v", err)
	}
	if f.process.starts != 0 {
		t.Fatal("invalid scope performed native mutation")
	}
}

func TestScopedAccessDoesNotReplayAmbiguousExecOrRefreshRejectedConnect(t *testing.T) {
	f := newAccessFixture(t)
	f.process.fail = true
	err := f.adapter.WithAccess(context.Background(), f.owner, func(s provider.Sandbox) error {
		_, err := s.Exec(context.Background(), f.owner, nil, "mutate")
		return err
	})
	var ambiguous *IndeterminateExecError
	if !errors.As(err, &ambiguous) || f.process.starts != 1 || f.finds != 1 || f.connects != 1 {
		t.Fatalf("ambiguous exec must not replay: %v counts=%d/%d/%d", err, f.finds, f.connects, f.process.starts)
	}
	f.rejectConnect = true
	called := false
	err = f.adapter.WithAccess(context.Background(), f.owner, func(provider.Sandbox) error { called = true; return nil })
	if err == nil || called || f.connects != 2 {
		t.Fatal("rejected fresh connection retried or called callback")
	}
}

func TestScopedAccessRequiresFreshExactProviderOwnership(t *testing.T) {
	for _, mode := range []string{"missing", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			f := newAccessFixture(t)
			f.missing, f.foreign = mode == "missing", mode == "foreign"
			err := f.adapter.WithAccess(context.Background(), f.owner, func(provider.Sandbox) error { t.Fatal("unowned resource entered scope"); return nil })
			if err == nil || f.connects != 0 {
				t.Fatalf("ownership failure=%v connects=%d", err, f.connects)
			}
		})
	}
}

func BenchmarkE2BAccessOperations(b *testing.B) {
	for _, scoped := range []bool{false, true} {
		name := "ordinary"
		if scoped {
			name = "scoped"
		}
		b.Run(name, func(b *testing.B) {
			f := newAccessFixture(b)
			for b.Loop() {
				var err error
				if scoped {
					err = f.adapter.WithAccess(context.Background(), f.owner, func(s provider.Sandbox) error { return accessSequence(context.Background(), f.owner, s) })
				} else {
					err = accessSequence(context.Background(), f.owner, f.adapter)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(f.finds)/float64(b.N), "lookup/op")
			b.ReportMetric(float64(f.connects)/float64(b.N), "connect/op")
			b.ReportMetric(float64(f.process.starts)/float64(b.N), "exec/op")
		})
	}
}

func TestScopedAccessDrainsCancelledInflightOperation(t *testing.T) {
	f := newAccessFixture(t)
	f.process.started = make(chan struct{})
	f.process.stopped = make(chan struct{})
	finished := make(chan error, 1)
	if err := f.adapter.WithAccess(context.Background(), f.owner, func(s provider.Sandbox) error {
		go func() {
			_, err := s.Exec(context.Background(), f.owner, nil, "wait")
			finished <- err
		}()
		<-f.process.started
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.process.stopped:
	default:
		t.Fatal("scope returned before its cancelled transport stopped")
	}
	if err := <-finished; err == nil {
		t.Fatal("cancelled in-flight command succeeded")
	}
	if f.process.starts != 1 {
		t.Fatal("cancelled command replayed")
	}
}

func TestScopedAccessRejectsTimerExpiredOperations(t *testing.T) {
	f := newAccessFixture(t)
	f.adapter.Config.SandboxTimeout = time.Second
	if err := f.adapter.WithAccess(context.Background(), f.owner, func(s provider.Sandbox) error {
		// Wait for the actual scope timer while its callback is still active.
		<-s.(*scopedAccess).ctx.Done()
		if _, err := s.Exec(context.Background(), f.owner, nil, "mutate"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timer-expired command: %v", err)
		}
		if _, err := s.Endpoint(context.Background(), f.owner, 4500); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timer-expired endpoint: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if f.process.starts != 0 || f.connects != 1 {
		t.Fatalf("expired access performed work or refreshed: exec=%d connect=%d", f.process.starts, f.connects)
	}
}
