package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/spine"
)

type profileGuardStore struct {
	job               spine.Job
	workflowAttention string
	cleanupAttention  string
	attentionErr      error
}

func (s *profileGuardStore) Job(context.Context, string) (spine.Job, error) { return s.job, nil }
func (s *profileGuardStore) SetWorkflowAttention(_ context.Context, _, _, detail string) error {
	s.workflowAttention = detail
	return s.attentionErr
}
func (s *profileGuardStore) SetCleanupAttention(_ context.Context, _, detail string) error {
	s.cleanupAttention = detail
	return s.attentionErr
}

func TestSandboxProfileGuardStopsMismatchedWorkAndCleanup(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		store := &profileGuardStore{job: spine.Job{ID: "job-1", SandboxProfile: "incus"}}
		err := requireSandboxProfile(context.Background(), store, store.job.ID, "e2b", cleanup)
		if err == nil || !strings.Contains(err.Error(), `requires Sandbox profile "incus"`) {
			t.Fatalf("cleanup=%v mismatch error=%v", cleanup, err)
		}
		if cleanup && store.cleanupAttention == "" || !cleanup && store.workflowAttention == "" {
			t.Fatalf("cleanup=%v did not persist profile attention: %#v", cleanup, store)
		}
	}
	matching := &profileGuardStore{job: spine.Job{ID: "job-1", SandboxProfile: "e2b"}}
	if err := requireSandboxProfile(context.Background(), matching, matching.job.ID, "e2b", false); err != nil {
		t.Fatal(err)
	}
	if matching.workflowAttention != "" || matching.cleanupAttention != "" {
		t.Fatalf("matching profile wrote attention: %#v", matching)
	}
	failedAttention := &profileGuardStore{job: spine.Job{ID: "job-1", SandboxProfile: "incus"}, attentionErr: errors.New("write failed")}
	if err := requireSandboxProfile(context.Background(), failedAttention, failedAttention.job.ID, "e2b", false); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("attention persistence error=%v", err)
	}
}

func TestPersistedWorkflowContractsV1(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"task result", TaskResultV1{JobID: "job-1", Outcome: "accepted"}, `{"job_id":"job-1","outcome":"accepted"}`},
		{"wake", WakeV1{JobID: "job-1", Sequence: 2}, `{"job_id":"job-1","sequence":2}`},
		{"fact step result", FactStepResultV1{FactID: "action-1"}, `{"fact_id":"action-1"}`},
		{"Action step result", ActionStepResultV1{ActionID: "action-1"}, `{"action_id":"action-1"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil || string(encoded) != test.want {
				t.Fatalf("persisted JSON = %s, want %s: %v", encoded, test.want, err)
			}
		})
	}
}

func TestWakeEventIsStableAndFIFOScoped(t *testing.T) {
	if WakeEvent("job-a", 2) != WakeEvent("job-a", 2) {
		t.Fatal("same admitted FIFO position did not retain its wake identity")
	}
	if WakeEvent("job-a", 2) == WakeEvent("job-b", 2) {
		t.Fatal("distinct Jobs share a wake event")
	}
	if WakeEvent("job-a", 2) == WakeEvent("job-a", 3) {
		t.Fatal("distinct FIFO positions share an immutable Absurd event")
	}
}

func TestActiveAgentObservationUsesInterruptiblePoll(t *testing.T) {
	options := wakeOptions(Work{Kind: WorkObserveAgent, FactID: "run-1"}, 2, 30*time.Second)
	if options.StepName != "dorf/agent-run-wake/v1/run-1/00000000000000000002" {
		t.Fatalf("active AgentRun wake step = %q", options.StepName)
	}
	if options.Timeout != time.Second {
		t.Fatalf("active AgentRun poll = %s, want 1s", options.Timeout)
	}
}

func TestStepNamesComeFromDurableFactIdentity(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"Action", actionStepName("action-1"), "dorf/action/v1/action-1"},
		{"setup Action", actionStepName("setup-1"), "dorf/action/v1/setup-1"},
		{"AgentRun", agentRunStepName("run-1"), "dorf/agent-run/v1/run-1"},
		{"Revision", revisionStepName("run-1"), "dorf/revision/v1/run-1"},
		{"Check", checkStepName("check-1"), "dorf/check/v1/check-1"},
		{"ReviewPolicy", reviewPolicyStepName("job-1", "revision-1"), "dorf/review-policy/v1/job-1/revision-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("Step name %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestCleanupActionOrderIsExactAndStable(t *testing.T) {
	targets := cleanupTargets([]spine.Sandbox{
		{ID: "sandbox-b", JobID: "job-1"},
		{ID: "sandbox-a", JobID: "job-1"},
	})
	want := []struct {
		sandbox string
		kind    spine.ActionKind
	}{
		{"sandbox-a", spine.ActionRouteRevoke},
		{"sandbox-a", spine.ActionSandboxDelete},
		{"sandbox-b", spine.ActionRouteRevoke},
		{"sandbox-b", spine.ActionSandboxDelete},
	}
	if len(targets) != len(want) {
		t.Fatalf("cleanup targets = %d, want %d", len(targets), len(want))
	}
	for i := range want {
		if targets[i].Sandbox.ID != want[i].sandbox || targets[i].Kind != want[i].kind {
			t.Fatalf("cleanup target %d = %#v, want %s %s", i, targets[i], want[i].sandbox, want[i].kind)
		}
	}
}
