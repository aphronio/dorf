package spine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	policy "github.com/aphronio/dorf/internal/review"
)

func TestReviewConcurrencyRequiresDistinctImmutableReadOnlyInputs(t *testing.T) {
	revision := strings.Repeat("a", 40)
	independent := []AgentRun{
		{ID: "run-a", Revision: revision, Capability: ReviewReadOnlyCapability, Workspace: "/tmp/review-a"},
		{ID: "run-b", Revision: revision, Capability: ReviewReadOnlyCapability, Workspace: "/tmp/review-b"},
	}
	if err := validateIndependentReviewBatch(independent); err != nil {
		t.Fatal(err)
	}
	shared := append([]AgentRun(nil), independent...)
	shared[1].Workspace = shared[0].Workspace
	if err := validateIndependentReviewBatch(shared); err == nil {
		t.Fatal("shared mutable workspace was considered independent")
	}
	mutable := append([]AgentRun(nil), independent...)
	mutable[1].Capability = "workspace-write"
	if err := validateIndependentReviewBatch(mutable); err == nil {
		t.Fatal("mutable capability was considered concurrently independent")
	}
}

func TestRevisionRoleReviewIdentityIsStableAndDistinct(t *testing.T) {
	revision := strings.Repeat("b", 40)
	first := ReviewAgentRunID("job-a", revision, "browser-ui")
	if first != ReviewAgentRunID("job-a", revision, "browser-ui") || first == ReviewAgentRunID("job-a", revision, "auth-authority") || first == ReviewAgentRunID("job-a", strings.Repeat("c", 40), "browser-ui") {
		t.Fatal("Revision/Role review identity is unstable or aliases another run")
	}
}

func TestReviewLostResponseReusesDistinctNativeSessionAndTurn(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	job.SessionID = "implementation-session"
	base.jobs[job.ID] = job
	run := AgentRun{ID: ReviewAgentRunID(job.ID, job.Revision, string(policy.RoleCriticalBoundary)), JobID: job.ID, Revision: job.Revision, Role: string(policy.RoleCriticalBoundary), Capability: ReviewReadOnlyCapability, Workspace: "/tmp/dorf/review-workspaces/run", InputContract: "bounded", OutputContract: policy.FindingOutputContract, State: AgentRunPending}
	run.ActionID = ScopedActionID(job.ID, ActionTurnStart, run.ID)
	workspaceAction := Action{ID: ScopedActionID(job.ID, ActionReviewWorkspaceCreate, run.ID), JobID: job.ID, Kind: ActionReviewWorkspaceCreate, Scope: run.ID, State: ActionSucceeded}
	sessionAction := Action{ID: ScopedActionID(job.ID, ActionSessionStart, run.ID), JobID: job.ID, Kind: ActionSessionStart, Scope: run.ID, State: ActionPending}
	base.runs[run.ID] = run
	base.actions[run.ActionID] = Action{ID: run.ActionID, JobID: job.ID, Kind: ActionTurnStart, Scope: run.ID, State: ActionPending}
	base.actions[workspaceAction.ID], base.actions[sessionAction.ID] = workspaceAction, sessionAction
	store := &reviewRecoveryStore{memoryStore: base, runID: run.ID, workspaceActionID: workspaceAction.ID, sessionActionID: sessionAction.ID}
	externals := &reviewRecoveryExternals{}
	service := Service{Store: store, Barrier: &failBarrier{point: BarrierAfterSubmitBeforeBind}}
	if _, err := service.executeReviewRun(context.Background(), job, run, externals, store); !errors.Is(err, errBarrier) {
		t.Fatalf("first review execution error=%v", err)
	}
	checkpoint := store.runs[run.ID]
	if externals.submissions != 1 || checkpoint.SessionID != "" || checkpoint.NativeTurnID != "" || !checkpoint.BaselineRecorded {
		t.Fatalf("lost-response checkpoint run=%#v submissions=%d", checkpoint, externals.submissions)
	}
	service.Barrier = nil
	outcome, err := service.executeReviewRun(context.Background(), job, run, externals, store)
	if err != nil {
		t.Fatal(err)
	}
	recovered := store.runs[run.ID]
	if externals.submissions != 1 || outcome.Status != "completed" || recovered.State != AgentRunCompleted || recovered.SessionID != "review-session" || recovered.SessionID == job.SessionID || recovered.NativeTurnID != "review-turn" {
		t.Fatalf("recovered run=%#v outcome=%#v submissions=%d", recovered, outcome, externals.submissions)
	}
}

type reviewRecoveryStore struct {
	*memoryStore
	runID, workspaceActionID, sessionActionID string
}

func (s *reviewRecoveryStore) CompleteAction(_ context.Context, id string, receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[id]
	action.State, action.ExternalID = ActionSucceeded, receipt.ExternalID
	s.actions[id] = action
	if id == s.sessionActionID {
		run := s.runs[s.runID]
		run.SessionID = receipt.ExternalID
		s.runs[s.runID] = run
	}
	return nil
}

func (s *reviewRecoveryStore) MarkChecksVerified(context.Context, string, string, []string) error {
	return nil
}
func (s *reviewRecoveryStore) ActivateReview(context.Context, ReviewActivation) (ReviewPlanRecord, bool, error) {
	return ReviewPlanRecord{}, false, nil
}
func (s *reviewRecoveryStore) ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error) {
	return ReviewPlanRecord{}, nil
}
func (s *reviewRecoveryStore) RecordReviewPolicy(context.Context, ReviewPlanRecord) error { return nil }
func (s *reviewRecoveryStore) ReviewRuns(context.Context, string, string) ([]ReviewRunView, error) {
	return nil, nil
}
func (s *reviewRecoveryStore) AllReviewRuns(context.Context, string) ([]ReviewRunView, error) {
	return nil, nil
}
func (s *reviewRecoveryStore) BeginReviewWorkspace(_ context.Context, _ string) (Action, error) {
	return s.actions[s.workspaceActionID], nil
}
func (s *reviewRecoveryStore) BeginReviewSession(_ context.Context, _ string) (Action, error) {
	return s.actions[s.sessionActionID], nil
}
func (s *reviewRecoveryStore) ReviewRun(_ context.Context, _ string) (AgentRun, error) {
	return s.runs[s.runID], nil
}
func (s *reviewRecoveryStore) RecordReviewResult(context.Context, string, NativeTurn, Evidence, Evidence, ReviewFinding) error {
	return nil
}
func (s *reviewRecoveryStore) RecordTriageResult(context.Context, string, NativeTurn, Evidence, Evidence, policy.ReviewPlan, string) error {
	return nil
}
func (s *reviewRecoveryStore) AdmitReviewRepair(context.Context, string, string) (Message, bool, error) {
	return Message{}, false, nil
}
func (s *reviewRecoveryStore) MarkReviewReady(context.Context, string, string) error { return nil }
func (s *reviewRecoveryStore) BeginReviewWorkspaceCleanup(context.Context, string) (Action, error) {
	return Action{}, nil
}
func (s *reviewRecoveryStore) ReviewRepairTargets(context.Context, string) ([]policy.Role, error) {
	return nil, nil
}
func (s *reviewRecoveryStore) RejectReviewFinding(context.Context, string, string) error { return nil }

type reviewRecoveryExternals struct {
	mu          sync.Mutex
	submissions int
	turn        NativeTurn
}

func (e *reviewRecoveryExternals) RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error) {
	return policy.ChangeFacts{}, nil
}
func (e *reviewRecoveryExternals) ReviewWorkspaceCreate(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, errors.New("workspace should already be ready")
}
func (e *reviewRecoveryExternals) ReviewWorkspaceDelete(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewRecoveryExternals) ReviewInitialTurn(context.Context, Job, AgentRun) (string, NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.turn.ID == "" {
		e.submissions++
		e.turn = NativeTurn{ID: "review-turn", Status: "running"}
	}
	return "review-session", e.turn, nil
}
func (e *reviewRecoveryExternals) ReviewTurns(context.Context, Job, AgentRun) ([]NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return []NativeTurn{e.turn}, nil
}
func (e *reviewRecoveryExternals) ReviewWait(context.Context, Job, AgentRun, string) (NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turn.Status = "completed"
	e.turn.Output = `{"material":false,"summary":"clear","rationale":"clear","affected_roles":[],"affected_checks":[]}`
	return e.turn, nil
}
