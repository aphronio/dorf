package spine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	policy "github.com/aphronio/dorf/internal/review"
)

func TestReviewConcurrencyRequiresDistinctImmutableReadOnlyInputs(t *testing.T) {
	revision := strings.Repeat("a", 40)
	independent := []AgentRun{
		{ID: "run-a", Revision: revision, Capability: ReviewReadOnlyCapability, Workspace: "/workspace/job", ReviewerSandboxID: "review-a", ReviewerRouteID: "route-a", ReviewerSandboxState: "created", ReviewerRouteState: "active", CheckoutState: "verified"},
		{ID: "run-b", Revision: revision, Capability: ReviewReadOnlyCapability, Workspace: "/workspace/job", ReviewerSandboxID: "review-b", ReviewerRouteID: "route-b", ReviewerSandboxState: "created", ReviewerRouteState: "active", CheckoutState: "verified"},
	}
	if err := validateIndependentReviewBatch(independent); err != nil {
		t.Fatal(err)
	}
	shared := append([]AgentRun(nil), independent...)
	shared[1].ReviewerSandboxID = shared[0].ReviewerSandboxID
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

func TestReviewControllerIdentityIsStableAndBoundToExactOwnership(t *testing.T) {
	runID, sandbox := "agent-run-review", "dorf-review-owned"
	nonce := strings.Repeat("a", 64)
	first := ReviewControllerID(runID, sandbox, nonce)
	if first == "" || first != ReviewControllerID(runID, sandbox, nonce) {
		t.Fatalf("logical reviewer controller identity is not stable: %q", first)
	}
	for _, foreign := range []string{
		ReviewControllerID("agent-run-foreign", sandbox, nonce),
		ReviewControllerID(runID, "dorf-review-foreign", nonce),
		ReviewControllerID(runID, sandbox, strings.Repeat("b", 64)),
	} {
		if foreign == first {
			t.Fatalf("foreign reviewer ownership reused logical identity %q", first)
		}
	}
}

func TestRejectedMaterialFindingSettlesButPendingOrAcceptedRetainsAuthority(t *testing.T) {
	for _, test := range []struct {
		name         string
		adjudication string
		repairCount  int
		wantReady    bool
		wantRepair   bool
		wantBlocked  bool
	}{
		{name: "rejected settles", adjudication: "rejected", repairCount: 1, wantReady: true},
		{name: "pending admits repair", adjudication: "pending", wantRepair: true},
		{name: "pending after repair blocks", adjudication: "pending", repairCount: 1, wantBlocked: true},
		{name: "accepted after repair blocks", adjudication: "accepted", repairCount: 1, wantBlocked: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newMemoryStore()
			job := testJob()
			job.WorkflowPhase = "reviewing"
			job.ReviewRepairCount = test.repairCount
			base.jobs[job.ID] = job
			store := newReviewDecisionStore(base)
			store.plan = ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, State: "final", Final: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.RoleCriticalBoundary}}}
			store.reviewRuns = []ReviewRunView{{AgentRun: AgentRun{ID: "review-run", JobID: job.ID, Revision: job.Revision, Role: string(policy.RoleCriticalBoundary), State: AgentRunCompleted}, Finding: &ReviewFinding{RunID: "review-run", Revision: job.Revision, Role: policy.RoleCriticalBoundary, Material: true, Adjudication: test.adjudication}}}

			disposition, progressed, err := (Service{Store: store}).executeSelectedReviews(context.Background(), job, store, nil)
			if err != nil {
				t.Fatal(err)
			}
			blocked := store.jobs[job.ID].WorkflowPhase == "blocked"
			if disposition == RunBlocked != test.wantBlocked || !progressed && !test.wantBlocked || store.ready != test.wantReady || store.repairs != boolInt(test.wantRepair) || blocked != test.wantBlocked {
				t.Fatalf("disposition=%s progressed=%t ready=%t repairs=%d blocked=%t", disposition, progressed, store.ready, store.repairs, blocked)
			}
		})
	}
}

func TestReviewPhasesAdvanceBeforeImplementationFIFO(t *testing.T) {
	for _, phase := range []string{"review-planning", "review-triage", "reviewing"} {
		t.Run(phase, func(t *testing.T) {
			base := newMemoryStore()
			job := testJob()
			job.WorkflowPhase, job.SessionID = phase, "implementation-session"
			base.jobs[job.ID] = job
			base.addMessage(job.ID, "queued-implementation-message", "must not dispatch during review")
			for _, kind := range []ActionKind{ActionSandboxCreate, ActionRepositoryClone, ActionRepositorySetup, ActionRouteCreate} {
				base.actions[ActionID(job.ID, kind)] = Action{ID: ActionID(job.ID, kind), JobID: job.ID, Kind: kind, State: ActionSucceeded, ExternalID: "ready-" + string(kind)}
			}
			store := newReviewDecisionStore(base)
			store.planErr = errReviewPhaseAdvanced
			externals := &reviewDispatchExternals{fakeExternals: newFakeExternals()}
			_, err := (Service{Store: store, Externals: externals, Repository: &fakeRepository{}}).RunUntilIdle(context.Background(), job.ID)
			if !errors.Is(err, errReviewPhaseAdvanced) {
				t.Fatalf("dispatch error=%v", err)
			}
			if store.nextDeliveryCalls != 0 {
				t.Fatalf("NextDelivery queried %d times during %s", store.nextDeliveryCalls, phase)
			}
		})
	}
}

func TestRepairPlanningUsesMandatoryFloorAndAffectedTargetsOnly(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	job.WorkflowPhase = "review-planning"
	job.ReviewRepairCount = 1
	base.jobs[job.ID] = job
	store := newReviewDecisionStore(base)
	store.plan = ReviewPlanRecord{
		JobID: job.ID, Revision: job.Revision, State: "pending",
		RequestedRoles: []policy.Role{policy.RoleBrowserUI, policy.RolePerformance, policy.RoleCriticalBoundary},
	}
	store.repairTargets = []policy.Role{policy.RoleCriticalBoundary}
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), job.Revision, []string{"internal/auth/session.go"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	externals := &reviewDispatchExternals{fakeExternals: newFakeExternals(), facts: facts}
	disposition, progressed, err := (Service{Store: store, Externals: externals}).advanceReview(context.Background(), job)
	if err != nil || disposition != RunIdle || !progressed {
		t.Fatalf("disposition=%s progressed=%t err=%v", disposition, progressed, err)
	}
	wantRoles := []policy.Role{policy.RoleAuthAuthority, policy.RoleCriticalBoundary}
	wantReasons := []policy.Reason{
		{Role: policy.RoleAuthAuthority, Source: "mandatory", Detail: "authentication or authority paths changed"},
		{Role: policy.RoleCriticalBoundary, Source: "accepted-finding", Detail: "accepted material finding invalidated this Role's claim"},
	}
	if !reflect.DeepEqual(store.recordedPolicy.Initial.Roles, wantRoles) || !reflect.DeepEqual(store.recordedPolicy.Initial.Reasons, wantReasons) || store.recordedPolicy.Initial.NeedsTriage {
		t.Fatalf("repair policy=%#v want Roles=%v Reasons=%v without triage", store.recordedPolicy.Initial, wantRoles, wantReasons)
	}
	for _, reason := range store.recordedPolicy.Initial.Reasons {
		if reason.Source == "implementation-request" {
			t.Fatalf("repair retained optional provenance: %#v", store.recordedPolicy.Initial.Reasons)
		}
	}
}

var errReviewPhaseAdvanced = errors.New("review phase reached before FIFO")

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

type reviewDecisionStore struct {
	*codingMemoryStore
	plan              ReviewPlanRecord
	reviewRuns        []ReviewRunView
	planErr           error
	recordedPolicy    ReviewPlanRecord
	repairTargets     []policy.Role
	nextDeliveryCalls int
	repairs           int
	ready             bool
}

func newReviewDecisionStore(base *memoryStore) *reviewDecisionStore {
	return &reviewDecisionStore{codingMemoryStore: &codingMemoryStore{memoryStore: base, checks: map[string]Check{}, evidence: map[string]Evidence{}}}
}

func (s *reviewDecisionStore) NextDelivery(ctx context.Context, jobID, sessionID string) (*Delivery, error) {
	s.nextDeliveryCalls++
	return s.memoryStore.NextDelivery(ctx, jobID, sessionID)
}
func (s *reviewDecisionStore) MarkChecksVerified(context.Context, string, string, []string) error {
	return nil
}
func (s *reviewDecisionStore) ActivateReview(context.Context, ReviewActivation) (ReviewPlanRecord, bool, error) {
	return ReviewPlanRecord{}, false, nil
}
func (s *reviewDecisionStore) ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error) {
	return s.plan, s.planErr
}
func (s *reviewDecisionStore) RecordReviewPolicy(_ context.Context, record ReviewPlanRecord) error {
	s.recordedPolicy = record
	return nil
}
func (s *reviewDecisionStore) ReviewRuns(context.Context, string, string) ([]ReviewRunView, error) {
	return append([]ReviewRunView(nil), s.reviewRuns...), nil
}
func (s *reviewDecisionStore) AllReviewRuns(context.Context, string) ([]ReviewRunView, error) {
	return append([]ReviewRunView(nil), s.reviewRuns...), nil
}
func (s *reviewDecisionStore) BeginReviewSandbox(context.Context, string) (Action, error) {
	return Action{}, fmt.Errorf("unexpected review Sandbox")
}
func (s *reviewDecisionStore) BeginReviewRoute(context.Context, string) (Action, error) {
	return Action{}, fmt.Errorf("unexpected review route")
}
func (s *reviewDecisionStore) BeginReviewWorkspace(context.Context, string) (Action, error) {
	return Action{}, fmt.Errorf("unexpected review workspace")
}
func (s *reviewDecisionStore) BeginReviewSession(context.Context, string) (Action, error) {
	return Action{}, fmt.Errorf("unexpected review Session")
}
func (s *reviewDecisionStore) ReviewRun(context.Context, string) (AgentRun, error) {
	return AgentRun{}, fmt.Errorf("unexpected review run")
}
func (s *reviewDecisionStore) RecordReviewResult(context.Context, string, NativeTurn, Evidence, Evidence, ReviewFinding) error {
	return nil
}
func (s *reviewDecisionStore) RecordTriageResult(context.Context, string, NativeTurn, Evidence, Evidence, policy.ReviewPlan, string) error {
	return nil
}
func (s *reviewDecisionStore) AdmitReviewRepair(context.Context, string, string) (Message, bool, error) {
	s.repairs++
	return Message{}, true, nil
}
func (s *reviewDecisionStore) MarkReviewReady(context.Context, string, string) error {
	s.ready = true
	return nil
}
func (s *reviewDecisionStore) BeginReviewWorkspaceCleanup(context.Context, string) (Action, error) {
	return s.reviewCleanupAction(ActionReviewWorkspaceDelete), nil
}
func (s *reviewDecisionStore) BeginReviewRouteCleanup(context.Context, string) (Action, error) {
	return s.reviewCleanupAction(ActionRouteRevoke), nil
}
func (s *reviewDecisionStore) BeginReviewSandboxCleanup(context.Context, string) (Action, error) {
	return s.reviewCleanupAction(ActionSandboxDelete), nil
}
func (s *reviewDecisionStore) InterruptReviewRun(_ context.Context, runID, reason string) error {
	for i := range s.reviewRuns {
		if s.reviewRuns[i].ID == runID {
			s.reviewRuns[i].State = AgentRunInterrupted
			s.reviewRuns[i].Attention = reason
		}
	}
	return nil
}
func (s *reviewDecisionStore) RecordReviewPostState(context.Context, string, Receipt) error {
	return nil
}
func (s *reviewDecisionStore) ReviewRepairTargets(context.Context, string) ([]policy.Role, error) {
	return append([]policy.Role(nil), s.repairTargets...), nil
}
func (s *reviewDecisionStore) RejectReviewFinding(context.Context, string, string) error { return nil }

func (s *reviewDecisionStore) reviewCleanupAction(kind ActionKind) Action {
	for _, action := range s.actions {
		if action.Kind == kind && action.Scope != "" {
			return action
		}
	}
	return Action{}
}

type reviewDispatchExternals struct {
	*fakeExternals
	facts          policy.ChangeFacts
	cleanupEffects []ActionKind
}

func (e *reviewDispatchExternals) RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error) {
	return e.facts, nil
}
func (e *reviewDispatchExternals) ReviewSandboxCreate(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewDispatchExternals) ReviewRouteCreate(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewDispatchExternals) ReviewWorkspaceCreate(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewDispatchExternals) ReviewWorkspaceDelete(context.Context, Job, AgentRun, Action) (Receipt, error) {
	e.cleanupEffects = append(e.cleanupEffects, ActionReviewWorkspaceDelete)
	return Receipt{}, nil
}
func (e *reviewDispatchExternals) ReviewWorkspaceVerify(context.Context, Job, AgentRun) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewDispatchExternals) ReviewRouteRevoke(context.Context, Job, AgentRun, Action) (Receipt, error) {
	e.cleanupEffects = append(e.cleanupEffects, ActionRouteRevoke)
	return Receipt{}, nil
}
func (e *reviewDispatchExternals) ReviewSandboxDelete(context.Context, Job, AgentRun, Action) (Receipt, error) {
	e.cleanupEffects = append(e.cleanupEffects, ActionSandboxDelete)
	return Receipt{}, nil
}

func TestUncertainReviewerResourceCleanupIsOrderedRecordedAndRetrySafe(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	job.AdmissionOpen = false
	job.CleanupState = CleanupScheduled
	base.jobs[job.ID] = job
	runID := ReviewAgentRunID(job.ID, job.Revision, string(policy.RoleCriticalBoundary))
	run := AgentRun{ID: runID, JobID: job.ID, Revision: job.Revision, Role: string(policy.RoleCriticalBoundary), State: AgentRunUncertain, Attention: "strict native identity mismatch", Workspace: "/workspace/job", ReviewerSandboxID: ReviewSandboxName(runID), ReviewerSandboxState: "created", ReviewerRouteState: "active", CheckoutState: "verified"}
	store := newReviewDecisionStore(base)
	store.reviewRuns = []ReviewRunView{{AgentRun: run, WorkspaceState: "created"}}
	for _, kind := range []ActionKind{ActionReviewWorkspaceDelete, ActionRouteRevoke, ActionSandboxDelete} {
		id := ScopedActionID(job.ID, kind, runID)
		base.actions[id] = Action{ID: id, JobID: job.ID, Kind: kind, Scope: runID, State: ActionPending}
	}
	externals := &reviewDispatchExternals{fakeExternals: newFakeExternals()}
	service := Service{Store: store, Externals: externals}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	wantReview := []ActionKind{ActionRouteRevoke, ActionReviewWorkspaceDelete, ActionSandboxDelete}
	if !reflect.DeepEqual(externals.cleanupEffects, wantReview) {
		t.Fatalf("review cleanup effects=%v want=%v", externals.cleanupEffects, wantReview)
	}
	if wantGlobal := []ActionKind{ActionRouteRevoke, ActionSandboxDelete}; !reflect.DeepEqual(externals.effects, wantGlobal) {
		t.Fatalf("implementation cleanup effects=%v want=%v", externals.effects, wantGlobal)
	}
	for _, kind := range wantReview {
		if action := store.reviewCleanupAction(kind); action.State != ActionSucceeded {
			t.Fatalf("review cleanup Action %s=%#v", kind, action)
		}
	}
	if got := store.reviewRuns[0]; got.State != AgentRunInterrupted || !strings.Contains(got.Attention, "resources are being reclaimed") {
		t.Fatalf("cleanup did not durably settle isolated reviewer run: %#v", got.AgentRun)
	}
}
func (e *reviewDispatchExternals) ReviewInitialTurn(context.Context, Job, AgentRun) (ReviewNativeBinding, error) {
	return ReviewNativeBinding{}, nil
}
func (e *reviewDispatchExternals) ReviewTurns(context.Context, Job, AgentRun) (ReviewNativeHistory, error) {
	return ReviewNativeHistory{}, nil
}
func (e *reviewDispatchExternals) ReviewWait(context.Context, Job, AgentRun, string) (ReviewNativeBinding, error) {
	return ReviewNativeBinding{}, nil
}

func TestReviewLostResponseReusesDistinctNativeSessionAndTurn(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	job.SessionID = "implementation-session"
	base.jobs[job.ID] = job
	run := AgentRun{ID: ReviewAgentRunID(job.ID, job.Revision, string(policy.RoleCriticalBoundary)), JobID: job.ID, Revision: job.Revision, Role: string(policy.RoleCriticalBoundary), Capability: ReviewReadOnlyCapability, Workspace: "/workspace/job", InputContract: "bounded", OutputContract: policy.FindingOutputContract, State: AgentRunPending, ReviewerSandboxState: "created", ReviewerRouteState: "active", CheckoutState: "verified", ReviewerOwnerNonce: strings.Repeat("2", 64), SubmissionNonce: strings.Repeat("1", 64), InputDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("bounded")))}
	run.ReviewerSandboxID = ReviewSandboxName(run.ID)
	run.ActionID = ScopedActionID(job.ID, ActionTurnStart, run.ID)
	sandboxAction := Action{ID: ScopedActionID(job.ID, ActionSandboxCreate, run.ID), JobID: job.ID, Kind: ActionSandboxCreate, Scope: run.ID, State: ActionSucceeded}
	routeAction := Action{ID: ScopedActionID(job.ID, ActionRouteCreate, run.ID), JobID: job.ID, Kind: ActionRouteCreate, Scope: run.ID, State: ActionSucceeded}
	workspaceAction := Action{ID: ScopedActionID(job.ID, ActionReviewWorkspaceCreate, run.ID), JobID: job.ID, Kind: ActionReviewWorkspaceCreate, Scope: run.ID, State: ActionSucceeded}
	sessionAction := Action{ID: ScopedActionID(job.ID, ActionSessionStart, run.ID), JobID: job.ID, Kind: ActionSessionStart, Scope: run.ID, State: ActionPending}
	base.runs[run.ID] = run
	base.actions[run.ActionID] = Action{ID: run.ActionID, JobID: job.ID, Kind: ActionTurnStart, Scope: run.ID, State: ActionPending}
	base.actions[sandboxAction.ID], base.actions[routeAction.ID], base.actions[workspaceAction.ID], base.actions[sessionAction.ID] = sandboxAction, routeAction, workspaceAction, sessionAction
	store := &reviewRecoveryStore{memoryStore: base, runID: run.ID, sandboxActionID: sandboxAction.ID, routeActionID: routeAction.ID, workspaceActionID: workspaceAction.ID, sessionActionID: sessionAction.ID}
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

type reviewAttentionTestError string

func (e reviewAttentionTestError) Error() string         { return string(e) }
func (e reviewAttentionTestError) AttentionNeeded() bool { return true }

func TestStrictReviewMismatchStopsWithAttentionAndNoClaimEvidence(t *testing.T) {
	for _, reason := range []string{"missing client message identity", "wrong prompt", "extra turn", "competing Session", "mismatched read-only policy", "mismatched reviewer-Sandbox metadata", "foreign logical controller identity"} {
		t.Run(reason, func(t *testing.T) {
			base := newMemoryStore()
			job := testJob()
			base.jobs[job.ID] = job
			run := AgentRun{ID: ReviewAgentRunID(job.ID, job.Revision, string(policy.RoleCriticalBoundary)), JobID: job.ID, Revision: job.Revision, Role: string(policy.RoleCriticalBoundary), Capability: ReviewReadOnlyCapability, Workspace: "/workspace/job", InputContract: "bounded", OutputContract: policy.FindingOutputContract, State: AgentRunPending, ReviewerSandboxState: "created", ReviewerRouteState: "active", CheckoutState: "verified", ReviewerOwnerNonce: strings.Repeat("2", 64), SubmissionNonce: strings.Repeat("1", 64), InputDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("bounded")))}
			run.ReviewerSandboxID = ReviewSandboxName(run.ID)
			run.ActionID = ScopedActionID(job.ID, ActionTurnStart, run.ID)
			actions := map[ActionKind]Action{
				ActionSandboxCreate:         {ID: ScopedActionID(job.ID, ActionSandboxCreate, run.ID), JobID: job.ID, Kind: ActionSandboxCreate, Scope: run.ID, State: ActionSucceeded},
				ActionRouteCreate:           {ID: ScopedActionID(job.ID, ActionRouteCreate, run.ID), JobID: job.ID, Kind: ActionRouteCreate, Scope: run.ID, State: ActionSucceeded},
				ActionReviewWorkspaceCreate: {ID: ScopedActionID(job.ID, ActionReviewWorkspaceCreate, run.ID), JobID: job.ID, Kind: ActionReviewWorkspaceCreate, Scope: run.ID, State: ActionSucceeded},
				ActionSessionStart:          {ID: ScopedActionID(job.ID, ActionSessionStart, run.ID), JobID: job.ID, Kind: ActionSessionStart, Scope: run.ID, State: ActionPending},
				ActionTurnStart:             {ID: run.ActionID, JobID: job.ID, Kind: ActionTurnStart, Scope: run.ID, State: ActionPending},
			}
			base.runs[run.ID] = run
			for _, action := range actions {
				base.actions[action.ID] = action
			}
			store := &reviewRecoveryStore{memoryStore: base, runID: run.ID, sandboxActionID: actions[ActionSandboxCreate].ID, routeActionID: actions[ActionRouteCreate].ID, workspaceActionID: actions[ActionReviewWorkspaceCreate].ID, sessionActionID: actions[ActionSessionStart].ID}
			externals := &reviewRecoveryExternals{initialErr: reviewAttentionTestError(reason)}
			if reason == "foreign logical controller identity" {
				externals.initialErr = nil
				externals.controllerID = "foreign-review-controller"
			}
			_, err := (Service{Store: store}).executeAndRecordReview(context.Background(), job, run, store, externals)
			settled := store.runs[run.ID]
			if err == nil || settled.State != AgentRunUncertain || !strings.Contains(settled.Attention, reason) || store.recordedResults != 0 || settled.ClaimEvidenceID != "" || settled.ObservedEvidenceID != "" {
				t.Fatalf("error=%v run=%#v recorded=%d", err, settled, store.recordedResults)
			}
		})
	}
}

type reviewRecoveryStore struct {
	*memoryStore
	runID, sandboxActionID, routeActionID, workspaceActionID, sessionActionID string
	recordedResults                                                           int
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
		run.ReviewerAppServer = receipt.Outcome
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
func (s *reviewRecoveryStore) BeginReviewSandbox(_ context.Context, _ string) (Action, error) {
	return s.actions[s.sandboxActionID], nil
}
func (s *reviewRecoveryStore) BeginReviewRoute(_ context.Context, _ string) (Action, error) {
	return s.actions[s.routeActionID], nil
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
	s.recordedResults++
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
func (s *reviewRecoveryStore) BeginReviewRouteCleanup(context.Context, string) (Action, error) {
	return Action{}, nil
}
func (s *reviewRecoveryStore) BeginReviewSandboxCleanup(context.Context, string) (Action, error) {
	return Action{}, nil
}
func (s *reviewRecoveryStore) InterruptReviewRun(context.Context, string, string) error { return nil }
func (s *reviewRecoveryStore) RecordReviewPostState(context.Context, string, Receipt) error {
	return nil
}
func (s *reviewRecoveryStore) ReviewRepairTargets(context.Context, string) ([]policy.Role, error) {
	return nil, nil
}
func (s *reviewRecoveryStore) RejectReviewFinding(context.Context, string, string) error { return nil }

type reviewRecoveryExternals struct {
	mu           sync.Mutex
	submissions  int
	turn         NativeTurn
	initialErr   error
	controllerID string
}

func (e *reviewRecoveryExternals) RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error) {
	return policy.ChangeFacts{}, nil
}
func (e *reviewRecoveryExternals) ReviewSandboxCreate(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, errors.New("Sandbox should already be ready")
}
func (e *reviewRecoveryExternals) ReviewRouteCreate(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, errors.New("route should already be ready")
}
func (e *reviewRecoveryExternals) ReviewWorkspaceCreate(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, errors.New("workspace should already be ready")
}
func (e *reviewRecoveryExternals) ReviewWorkspaceDelete(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewRecoveryExternals) ReviewWorkspaceVerify(context.Context, Job, AgentRun) (Receipt, error) {
	return Receipt{ExternalID: "/workspace/job", Outcome: strings.Repeat("a", 40) + " " + strings.Repeat("b", 40) + " clean"}, nil
}
func (e *reviewRecoveryExternals) ReviewRouteRevoke(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewRecoveryExternals) ReviewSandboxDelete(context.Context, Job, AgentRun, Action) (Receipt, error) {
	return Receipt{}, nil
}
func (e *reviewRecoveryExternals) reviewBinding(run AgentRun) ReviewNativeBinding {
	controllerID := e.controllerID
	if controllerID == "" {
		controllerID = ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
	}
	return ReviewNativeBinding{AppServerID: controllerID, SessionID: "review-session", Turn: e.turn}
}
func (e *reviewRecoveryExternals) reviewHistory(run AgentRun) ReviewNativeHistory {
	binding := e.reviewBinding(run)
	return ReviewNativeHistory{AppServerID: binding.AppServerID, SessionID: binding.SessionID, Turns: []NativeTurn{e.turn}}
}
func (e *reviewRecoveryExternals) ReviewInitialTurn(_ context.Context, _ Job, run AgentRun) (ReviewNativeBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.initialErr != nil {
		return ReviewNativeBinding{}, e.initialErr
	}
	if e.turn.ID == "" {
		e.submissions++
		e.turn = NativeTurn{ID: "review-turn", Status: "running"}
	}
	return e.reviewBinding(run), nil
}
func (e *reviewRecoveryExternals) ReviewTurns(_ context.Context, _ Job, run AgentRun) (ReviewNativeHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reviewHistory(run), nil
}
func (e *reviewRecoveryExternals) ReviewWait(_ context.Context, _ Job, run AgentRun, _ string) (ReviewNativeBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turn.Status = "completed"
	e.turn.Output = `{"material":false,"summary":"clear","rationale":"clear","affected_roles":[],"affected_checks":[]}`
	return e.reviewBinding(run), nil
}
