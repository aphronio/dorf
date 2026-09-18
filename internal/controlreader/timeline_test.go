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

func (r *timelineTestRuntime) ResolveSandbox(context.Context, core.SandboxProfileRef) (core.SandboxRuntime, error) {
	return core.SandboxRuntime{SandboxProfile: r.store.job.ProfileRef(), Timeline: r}, nil
}
func (r *timelineTestRuntime) ReadTimeline(_ context.Context, job core.Job, owned core.Sandbox, threadID, turnID string) (core.HarnessTimeline, error) {
	r.calls++
	if !r.store.inFence || job != r.store.job || owned != r.store.sandbox || threadID != "bound" || turnID != "selected" {
		return core.HarnessTimeline{}, errors.New("wrong read authority")
	}
	return r.result, r.err
}

func TestTimelineClientEnforcesCustodyAndPropagatesNativeFailures(t *testing.T) {
	job := core.Job{ID: "job", SandboxProfile: "profile", CleanupState: core.CleanupPending, ThreadHarness: "codex", ThreadID: "bound"}
	owned := core.Sandbox{ID: "sandbox", Name: core.DefaultSandbox, JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	store := &timelineTestStore{readerTestStore: &readerTestStore{job: job, sandbox: owned}}
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
	for _, test := range []string{"missing-job", "cleanup", "no-binding", "foreign-sandbox", "foreign-thread", "unknown-turn", "unsupported", "oversized"} {
		t.Run(test, func(t *testing.T) {
			store.job = job
			store.sandbox = owned
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
				store.job.ThreadHarness, store.job.ThreadID = "", ""
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

func TestMessageTimelineMapsOnlyInputsWithExactStoredTurnCustody(t *testing.T) {
	job := core.Job{ID: "job", SandboxProfile: "profile", CleanupState: core.CleanupPending}
	owned := core.Sandbox{ID: "sandbox", Name: "review-sandbox", JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	run := core.AgentRun{ID: "run", JobID: job.ID, MessageID: "message", SandboxID: owned.ID, Harness: "codex", ThreadID: "bound", TurnID: "selected", State: core.AgentRunActive}
	message := core.Message{ID: "message", JobID: job.ID}
	execution := core.AgentMessageExecution{Job: job, Message: message, AgentRun: run, Sandbox: owned}
	store := &timelineTestStore{readerTestStore: &readerTestStore{job: job, sandbox: owned, execution: execution}, deliveries: []core.Delivery{{Message: message, AgentRun: run}}}
	runtime := &timelineTestRuntime{store: store, result: core.HarnessTimeline{Harness: "codex", ThreadID: "bound", TurnID: "selected", Status: "inProgress", Items: []json.RawMessage{json.RawMessage(`{"id":"input","type":"userMessage"}`)}, CompletedItems: []core.HarnessConversationItem{{Index: 0, NativeItemID: "input", Kind: "input", ClientID: "run"}, {Index: 1, NativeItemID: "answer", Kind: "reply", Text: "[PDF](sandbox:/report.pdf)"}, {Index: 2, NativeItemID: "unknown", Kind: "input", ClientID: "not-ours"}}}}
	handler, err := NewHandler(strings.Repeat("b", 64), Service{Store: store, Runtimes: runtime})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient("http://reader.test:8756", strings.Repeat("b", 64), &http.Client{Transport: readerHandlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ReadMessageTimeline(context.Background(), job.ID, message.ID)
	if err != nil || len(result.CompletedItems) != 3 || result.CompletedItems[0].MessageID != message.ID || result.CompletedItems[0].ClientID != "" || result.CompletedItems[1].Text != "[PDF](sandbox:/report.pdf)" || result.CompletedItems[2].MessageID != "" || len(result.Items) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, invalid := range []string{"job", "message", "sandbox", "nonce", "thread", "turn", "harness", "cleanup", "stale-sandbox"} {
		t.Run(invalid, func(t *testing.T) {
			store.execution = execution
			store.sandbox = owned
			switch invalid {
			case "job":
				store.execution.AgentRun.JobID = "other"
			case "message":
				store.execution.AgentRun.MessageID = "other"
			case "sandbox":
				store.execution.AgentRun.SandboxID = "other"
			case "nonce":
				store.execution.Sandbox.OwnershipNonce = ""
			case "thread":
				store.execution.AgentRun.ThreadID = ""
			case "turn":
				store.execution.AgentRun.TurnID = ""
			case "harness":
				store.execution.AgentRun.Harness = ""
			case "cleanup":
				store.execution.Job.CleanupState = core.CleanupRequested
			case "stale-sandbox":
				store.sandbox.OwnershipNonce = strings.Repeat("c", 64)
			}
			before := runtime.calls
			_, err := client.ReadMessageTimeline(context.Background(), job.ID, message.ID)
			if !errors.Is(err, core.ErrTimelineUnavailable) || runtime.calls != before {
				t.Fatalf("err=%v calls=%d/%d", err, runtime.calls, before)
			}
		})
	}
}

func TestMessageTimelineDoesNotAttributeAnInputThroughForeignDelivery(t *testing.T) {
	run := core.AgentRun{ID: "run", JobID: "job", MessageID: "message", SandboxID: "sandbox", Harness: "codex", ThreadID: "thread", TurnID: "turn"}
	original := core.Delivery{Message: core.Message{ID: "message", JobID: "job"}, AgentRun: run}
	for _, field := range []string{"job", "message-job", "message", "sandbox", "harness", "thread", "turn"} {
		t.Run(field, func(t *testing.T) {
			delivery := original
			switch field {
			case "job":
				delivery.AgentRun.JobID = "other"
			case "message-job":
				delivery.Message.JobID = "other"
			case "message":
				delivery.AgentRun.MessageID = "other"
			case "sandbox":
				delivery.AgentRun.SandboxID = "other"
			case "harness":
				delivery.AgentRun.Harness = "other"
			case "thread":
				delivery.AgentRun.ThreadID = "other"
			case "turn":
				delivery.AgentRun.TurnID = "other"
			}
			result := core.HarnessTimeline{CompletedItems: []core.HarnessConversationItem{{Index: 0, Kind: "input", NativeItemID: "native", ClientID: "run", MessageID: "injected"}}}
			if err := bindTimelineInputs(&result, []core.Delivery{delivery}, run); err != nil {
				t.Fatal(err)
			}
			if result.CompletedItems[0].MessageID != "" || result.CompletedItems[0].ClientID != "" {
				t.Fatalf("foreign attribution=%+v", result)
			}
		})
	}
}
