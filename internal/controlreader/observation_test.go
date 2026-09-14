package controlreader

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
)

func observationFixture() (Service, *timelineTestStore, *timelineTestRuntime, codex.ReplyBinding) {
	job := core.Job{ID: "job", SandboxProfile: "profile", CleanupState: core.CleanupPending, AdmissionOpen: true}
	owned := core.Sandbox{ID: "sandbox", Name: core.DefaultSandbox, JobID: job.ID, OwnershipNonce: strings.Repeat("a", 64)}
	run := core.AgentRun{ID: "run", JobID: job.ID, MessageID: "message", SandboxID: owned.ID, Harness: "codex", ThreadID: "bound", TurnID: "selected", State: core.AgentRunActive}
	message := core.Message{ID: "message", JobID: job.ID, Intent: core.MessageFollow}
	store := &timelineTestStore{readerTestStore: &readerTestStore{job: job, sandbox: owned, execution: core.AgentMessageExecution{Job: job, Message: message, AgentRun: run, Sandbox: owned}}, deliveries: []core.Delivery{{Message: message, AgentRun: run}}}
	runtime := &timelineTestRuntime{store: store, result: core.HarnessTimeline{Harness: "codex", ThreadID: "bound", TurnID: "selected", Status: "inProgress", Items: []json.RawMessage{json.RawMessage(`{"id":"input","type":"userMessage"}`)}, CompletedItems: []core.HarnessConversationItem{{Index: 0, NativeItemID: "input", Kind: "input", ClientID: "run"}, {Index: 1, NativeItemID: "reply", Kind: "reply", Text: "answer"}}}}
	service := Service{Store: store, Runtimes: runtime, Replies: codex.NewReplyFeed()}
	binding := codex.ReplyBinding{JobID: job.ID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce, Harness: "codex", ThreadID: "bound", TurnID: "selected"}
	return service, store, runtime, binding
}

func TestObservationDefersMissingCacheAndRequiresOutcomeAndFinalPrefix(t *testing.T) {
	service, store, runtime, binding := observationFixture()
	input := observationRequest{JobID: "job", MessageID: "message"}
	value, _, err := service.messageObservation(context.Background(), input, false)
	if err != nil || value.State != "resync_deferred" || runtime.calls != 0 || store.activityStarts != 0 {
		t.Fatalf("stream hydrated idle cache: %+v %v", value, err)
	}
	value, err = service.ReadMessageObservation(context.Background(), "job", "message", "")
	if err != nil || runtime.calls != 1 || len(value.Items) != 2 || value.Items[0].MessageID != "message" || value.Items[0].ClientID != "" {
		t.Fatalf("explicit hydration: %+v %v", value, err)
	}
	input.Cursor = *value.Cursor
	store.execution.AgentRun.TurnOutcome = "completed"
	store.execution.AgentRun.State = core.AgentRunCompleted
	value, _, err = service.messageObservation(context.Background(), input, false)
	if err != nil || value.CompletionWatermark != nil || value.Outcome == nil || len(value.Items) != 0 {
		t.Fatalf("outcome alone completed prefix: %+v %v", value, err)
	}
	service.Replies.Seed(binding, []core.HarnessConversationItem{{Index: 0, NativeItemID: "cold-input", Kind: "input", ClientID: "run"}, {Index: 1, NativeItemID: "cold-reply", Kind: "reply", Text: "answer"}, {Index: 2, NativeItemID: "final", Kind: "reply", Text: "last answer"}}, true)
	value, _, err = service.messageObservation(context.Background(), input, false)
	if err != nil || value.State != "complete" || value.CompletionWatermark == nil || *value.CompletionWatermark != 3 || len(value.Items) != 1 || value.FromIndex != 2 || value.Items[0].Index != 2 {
		t.Fatalf("late final reply: %+v %v", value, err)
	}
	if runtime.calls != 1 || store.activityStarts != 1 {
		t.Fatal("passive stream touched native runtime or activity")
	}
}

func TestObservationCursorRejectsBindingDriftAndRewrittenPrefixAfterRestart(t *testing.T) {
	service, store, _, binding := observationFixture()
	value, err := service.ReadMessageObservation(context.Background(), "job", "message", "")
	if err != nil {
		t.Fatal(err)
	}
	input := observationRequest{JobID: "job", MessageID: "message", Cursor: *value.Cursor}
	store.execution.AgentRun.TurnID = "other"
	if _, _, err := service.messageObservation(context.Background(), input, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("binding drift error=%v", err)
	}
	store.execution.AgentRun.TurnID = "selected"
	service.Replies = codex.NewReplyFeed()
	value, _, err = service.messageObservation(context.Background(), input, false)
	if err != nil || value.State != "resync_deferred" || *value.Cursor != input.Cursor {
		t.Fatalf("restart lost resume position: %+v %v", value, err)
	}
	service.Replies.Seed(binding, []core.HarnessConversationItem{{Index: 0, NativeItemID: "new-input", Kind: "input", ClientID: "run"}, {Index: 1, NativeItemID: "new-reply", Kind: "reply", Text: "rewritten"}}, true)
	value, _, err = service.messageObservation(context.Background(), input, false)
	if err != nil || value.State != "gap" || len(value.Items) != 0 {
		t.Fatalf("cursor accepted changed consumed prefix: %+v %v", value, err)
	}
}

func TestObservationFailureBeforeNativeBindingIsCompleteWithoutHistory(t *testing.T) {
	service, store, runtime, _ := observationFixture()
	store.execution.AgentRun.ThreadID = ""
	store.execution.AgentRun.TurnID = ""
	store.execution.AgentRun.State = core.AgentRunFailed
	value, err := service.ReadMessageObservation(context.Background(), "job", "message", "")
	if err != nil || value.State != "complete" || value.Binding != nil || value.Cursor != nil || value.CompletionWatermark == nil || *value.CompletionWatermark != 0 || runtime.calls != 0 {
		t.Fatalf("unbound failure: %+v %v", value, err)
	}
}

func TestObservationNativeCompletionCannotOverrideActiveDurableState(t *testing.T) {
	service, store, runtime, binding := observationFixture()
	service.Replies.Seed(binding, runtime.result.CompletedItems, true)
	store.execution.AgentRun.TurnOutcome = "completed"
	value, _, err := service.messageObservation(context.Background(), observationRequest{JobID: "job", MessageID: "message"}, false)
	if err != nil || value.Outcome != nil || value.CompletionWatermark != nil || value.State != "observing" {
		t.Fatalf("native completion overrode active custody: %+v %v", value, err)
	}
}

func TestObservationHardJobAttentionOverridesDeliveryUncertainty(t *testing.T) {
	service, store, _, _ := observationFixture()
	store.execution.AgentRun.State = core.AgentRunUncertain
	store.execution.AgentRun.Attention = "submission acknowledgement was lost"

	tests := []struct {
		name       string
		configure  func()
		callback   func(context.Context, core.Job) (string, error)
		wantCode   string
		wantCalled bool
	}{
		{
			name: "closed job",
			configure: func() {
				store.execution.Job.AdmissionOpen = false
			},
			callback:   func(context.Context, core.Job) (string, error) { return "task_failed", nil },
			wantCode:   "job_closed",
			wantCalled: false,
		},
		{
			name: "workflow attention",
			configure: func() {
				store.execution.Job.WorkflowAttention = "workflow failed"
			},
			callback:   func(context.Context, core.Job) (string, error) { return "task_failed", nil },
			wantCode:   "job_attention",
			wantCalled: false,
		},
		{
			name:       "failed workflow task",
			configure:  func() {},
			callback:   func(context.Context, core.Job) (string, error) { return "task_failed", nil },
			wantCode:   "task_failed",
			wantCalled: true,
		},
		{
			name:       "delivery uncertainty",
			configure:  func() {},
			callback:   func(context.Context, core.Job) (string, error) { return "", nil },
			wantCode:   "agent_delivery_attention",
			wantCalled: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := store.execution.Job
			defer func() { store.execution.Job = job }()
			test.configure()
			called := false
			service.ObservationAttention = func(ctx context.Context, job core.Job) (string, error) {
				called = true
				return test.callback(ctx, job)
			}
			result := MessageObservation{}
			if err := service.projectObservationAttention(context.Background(), &result, store.execution); err != nil {
				t.Fatal(err)
			}
			if result.Attention == nil || result.Attention.Code != test.wantCode || called != test.wantCalled {
				t.Fatalf("attention=%+v callback_called=%v", result.Attention, called)
			}
		})
	}
}
