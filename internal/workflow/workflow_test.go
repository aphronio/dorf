package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/controlplane"
	"github.com/aphronio/dorf/internal/spine"
)

type profileGuardStore struct {
	job               spine.Job
	jobCalls          int
	workflowAttention string
	attentionErr      error
}

type profileRuntimeResolverStub struct {
	codingRuntime        CodingRuntime
	investigationRuntime InvestigationRuntime
	err                  error
	codingName           string
	investigationName    string
}

func (r *profileRuntimeResolverStub) ResolveCoding(_ context.Context, name string) (CodingRuntime, error) {
	r.codingName = name
	return r.codingRuntime, r.err
}

func (r *profileRuntimeResolverStub) ResolveInvestigation(_ context.Context, name string) (InvestigationRuntime, error) {
	r.investigationName = name
	return r.investigationRuntime, r.err
}

func (s *profileGuardStore) Job(context.Context, string) (spine.Job, error) {
	s.jobCalls++
	return s.job, nil
}

func TestCodingRuntimeResolutionUsesOnlyCodingAuthority(t *testing.T) {
	definition := CodingToProposalDefinition()
	store := &profileGuardStore{job: spine.Job{
		ID: "job-1", SandboxProfile: "managed", Workflow: definition.Name, WorkflowRevision: definition.Revision,
	}}
	resolver := &profileRuntimeResolverStub{codingRuntime: CodingRuntime{
		Runtime: Runtime{Profile: RuntimeProfile{SandboxProfile: "managed"}},
	}}
	runtime, err := codingRuntimeForJob(context.Background(), store, resolver, store.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.codingName != "managed" || runtime.Profile.SandboxProfile != "managed" {
		t.Fatalf("coding=%q runtime=%#v", resolver.codingName, runtime)
	}
}
func (s *profileGuardStore) SetWorkflowAttention(_ context.Context, _, _, detail string) error {
	s.workflowAttention = detail
	return s.attentionErr
}

func TestSandboxProfileGuardStopsMismatchedWork(t *testing.T) {
	definition := CodebaseInvestigationDefinition()
	store := &profileGuardStore{job: spine.Job{ID: "job-1", SandboxProfile: "incus"}}
	err := requireJobProfile(context.Background(), store, store.job, RuntimeProfile{SandboxProfile: "e2b"}, definition)
	if err == nil || !strings.Contains(err.Error(), `requires Sandbox profile "incus"`) {
		t.Fatalf("mismatch error=%v", err)
	}
	if store.workflowAttention == "" {
		t.Fatalf("profile mismatch did not persist attention: %#v", store)
	}
	matching := &profileGuardStore{job: spine.Job{ID: "job-1", SandboxProfile: "e2b"}}
	if err := requireJobProfile(context.Background(), matching, matching.job, RuntimeProfile{SandboxProfile: "e2b"}, definition); err != nil {
		t.Fatal(err)
	}
	if matching.workflowAttention != "" {
		t.Fatalf("matching profile wrote attention: %#v", matching)
	}
	failedAttention := &profileGuardStore{job: spine.Job{ID: "job-1", SandboxProfile: "incus"}, attentionErr: errors.New("write failed")}
	if err := requireJobProfile(context.Background(), failedAttention, failedAttention.job, RuntimeProfile{SandboxProfile: "e2b"}, definition); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("attention persistence error=%v", err)
	}
}

func TestPersistedWorkflowContractsV1(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"task result", controlplane.TaskResultV1{JobID: "job-1", Outcome: "accepted"}, `{"job_id":"job-1","outcome":"accepted"}`},
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
