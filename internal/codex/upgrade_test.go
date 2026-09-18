package codex

import (
	"context"
	"reflect"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestNativePersistenceReadinessDoesNotResumeThread(t *testing.T) {
	for _, test := range []struct {
		name   string
		resume bool
		want   []string
	}{
		{"persistence read", false, []string{"thread/read"}},
		{"upgrade compatibility", true, []string{"thread/resume", "thread/read"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, requests := testProtocolServer(t, func(method string, _ map[string]any) (map[string]any, bool) {
				switch method {
				case "initialize":
					return map[string]any{}, false
				case "thread/resume":
					return map[string]any{"thread": map[string]any{"id": "thread"}}, false
				case "thread/read":
					return map[string]any{"thread": map[string]any{"id": "thread", "turns": []any{map[string]any{"id": "turn", "status": "completed"}}}}, false
				default:
					t.Errorf("unexpected native operation %s", method)
					return nil, true
				}
			})
			defer server.Close()
			protocol := dialTestProtocol(t, server)
			expected := []core.AgentRun{{ThreadID: "thread", TurnID: "turn", TurnOutcome: "completed"}}
			if err := verifyRetainedThread(context.Background(), protocol, "thread", expected, test.resume); err != nil {
				t.Fatal(err)
			}
			if got := protocolMethods(requests); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("native readiness methods=%v, want %v", got, test.want)
			}
		})
	}
}

func TestUpgradeVerificationRequiresExactSettledNativeHistory(t *testing.T) {
	expected := []core.AgentRun{{ThreadID: "thread", TurnID: "turn", TurnOutcome: "completed"}}
	for _, tc := range []struct {
		name  string
		turns []TurnOutcome
		valid bool
	}{
		{"retained", []TurnOutcome{{ID: "turn", Status: "completed"}}, true},
		{"empty", nil, false},
		{"different", []TurnOutcome{{ID: "other", Status: "completed"}}, false},
		{"active", []TurnOutcome{{ID: "turn", Status: "running"}}, false},
		{"changed", []TurnOutcome{{ID: "turn", Status: "failed"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyRetainedTurns(expected, tc.turns); (err == nil) != tc.valid {
				t.Fatalf("verification error=%v", err)
			}
		})
	}
}

func TestQuiesceNeverStartsOrStopsAnUnownedServer(t *testing.T) {
	for _, test := range []struct {
		probe   string
		wantErr bool
	}{
		{"0\n0\n", false}, // absent: no route credentials or native startup needed
		{"1\n0\n", true},  // untracked: not an owned kill target
		{"1\n1\n", true},  // tracked but missing authentication
	} {
		sandbox := quiesceProbeSandbox{t: t, probe: test.probe}
		if err := (Agent{Sandbox: sandbox}).Quiesce(context.Background(), testOwner("quiesce"), nil); (err != nil) != test.wantErr {
			t.Fatalf("probe %q: %v", test.probe, err)
		}
	}
}

type quiesceProbeSandbox struct {
	provider.Sandbox
	t     *testing.T
	probe string
}

func (s quiesceProbeSandbox) Endpoint(context.Context, provider.Ownership, int) (provider.Endpoint, error) {
	return provider.Endpoint{ListenURL: "ws://127.0.0.1:4500"}, nil
}

func (s quiesceProbeSandbox) Exec(_ context.Context, _ provider.Ownership, _ []byte, args ...string) (provider.Result, error) {
	if !reflect.DeepEqual(args, []string{"bash", "-lc", probeServerScript("ws://127.0.0.1:4500")}) {
		s.t.Fatalf("quiesce attempted an unexpected process operation: %q", args)
	}
	return provider.Result{Stdout: s.probe}, nil
}
