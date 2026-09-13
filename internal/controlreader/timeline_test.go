package controlreader

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

type timelineTestStore struct {
	*readerTestStore
	deliveries []core.Delivery
}

func (s *timelineTestStore) Sandboxes(context.Context, string) ([]core.Sandbox, error) {
	return []core.Sandbox{s.sandbox}, nil
}
func (s *timelineTestStore) Deliveries(context.Context, string) ([]core.Delivery, error) {
	return s.deliveries, nil
}

type timelineTestRuntime struct {
	store  *timelineTestStore
	calls  int
	result core.HarnessTimeline
	err    error
}

func (r *timelineTestRuntime) ResolveSandbox(context.Context, string) (core.SandboxRuntime, error) {
	return core.SandboxRuntime{SandboxProfile: r.store.job.SandboxProfile, Timeline: r}, nil
}
func (r *timelineTestRuntime) ReadTimeline(_ context.Context, job core.Job, owned core.Sandbox, threadID, turnID string) (core.HarnessTimeline, error) {
	r.calls++
	if !r.store.inFence || job != r.store.job || owned != r.store.sandbox || threadID != "bound" || turnID != "selected" {
		return core.HarnessTimeline{}, errors.New("wrong read authority")
	}
	return r.result, r.err
}

func TestTimelineClientEnforcesCustodyAndPropagatesNativeFailures(t *testing.T) {
	job := core.Job{ID: "job", SandboxProfile: "profile", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox", Name: core.DefaultSandbox, JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	delivery := core.Delivery{Message: core.Message{ID: "message", JobID: job.ID}, AgentRun: core.AgentRun{ID: "run", JobID: job.ID, MessageID: "message", SandboxID: owned.ID, Harness: "codex", ThreadID: "bound"}}
	store := &timelineTestStore{readerTestStore: &readerTestStore{job: job, sandbox: owned}, deliveries: []core.Delivery{delivery}}
	runtime := &timelineTestRuntime{store: store, result: core.HarnessTimeline{Harness: "codex", ThreadID: "bound", TurnID: "selected", Status: "inProgress", Items: []json.RawMessage{json.RawMessage(`{"id":"item","type":"agentMessage","phase":"commentary"}`)}}}
	handler, err := NewHandler(strings.Repeat("b", 64), Service{Store: store, Runtimes: runtime})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://control-reader.test:8756", strings.Repeat("b", 64), &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ReadTimeline(context.Background(), job.ID, "selected")
	if err != nil || result.TurnID != "selected" || runtime.calls != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, test := range []string{"missing-job", "cleanup", "no-binding", "conflicting-binding", "foreign-job", "foreign-sandbox", "foreign-thread", "unknown-turn", "unsupported", "oversized"} {
		t.Run(test, func(t *testing.T) {
			store.job = job
			store.sandbox = owned
			store.deliveries = []core.Delivery{delivery}
			runtime.err = nil
			runtime.result.ThreadID = "bound"
			runtime.result.Items = []json.RawMessage{json.RawMessage(`{"id":"item","type":"agentMessage"}`)}
			jobID := job.ID
			want := core.ErrTimelineUnavailable
			switch test {
			case "missing-job":
				jobID = "missing"
				want = ErrJobNotFound
			case "cleanup":
				store.job.CleanupState = core.CleanupRequested
			case "no-binding":
				store.deliveries = nil
			case "conflicting-binding":
				other := delivery
				other.AgentRun.ThreadID = "other"
				store.deliveries = append(store.deliveries, other)
			case "foreign-job":
				store.deliveries[0].AgentRun.JobID = "foreign"
			case "foreign-sandbox":
				store.sandbox.JobID = "foreign"
			case "foreign-thread":
				runtime.result.ThreadID = "other"
			case "unknown-turn":
				runtime.err = core.ErrTurnNotFound
				want = core.ErrTurnNotFound
			case "unsupported":
				runtime.err = errors.New("unsupported native version")
			case "oversized":
				runtime.result.Items = []json.RawMessage{json.RawMessage(`{"text":"` + strings.Repeat("x", MaxObservationBytes) + `"}`)}
				want = ErrResponseTooLarge
			}
			result, err := client.ReadTimeline(context.Background(), jobID, "selected")
			if !errors.Is(err, want) || len(result.Items) != 0 {
				t.Fatalf("result=%+v err=%v want=%v", result, err, want)
			}
		})
	}
}
