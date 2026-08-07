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
	"time"

	"github.com/aphronio/dorf/internal/evidence"
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
func (e *reviewDispatchExternals) ReviewRecover(context.Context, Job, AgentRun) (ReviewNativeBinding, error) {
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
	store := &reviewRecoveryStore{codingMemoryStore: newCodingMemoryStore(base), runID: run.ID, sandboxActionID: sandboxAction.ID, routeActionID: routeAction.ID, workspaceActionID: workspaceAction.ID, sessionActionID: sessionAction.ID}
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

type reviewVisibilityTestError string

func (e reviewVisibilityTestError) Error() string                   { return string(e) }
func (e reviewVisibilityTestError) RetryableReviewVisibility() bool { return true }

func TestUncertainReviewSubmissionRecoversExactEffectWithoutResubmission(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	store := reviewRecoveryStoreFor(base, run)
	externals := &reviewRecoveryExternals{lostResponseOnce: true, store: store}
	service := Service{Store: store}

	if _, err := service.executeReviewRun(context.Background(), job, run, externals, store); err == nil || !strings.Contains(err.Error(), "response was lost") {
		t.Fatalf("lost response error=%v", err)
	}
	checkpoint := store.runs[run.ID]
	session := store.actions[store.sessionActionID]
	turnAction := store.actions[run.ActionID]
	if externals.submissions != 1 || checkpoint.State != AgentRunUncertain || checkpoint.SessionID != "" || checkpoint.NativeTurnID != "" || session.State != ActionUncertain || turnAction.State != ActionUncertain || !strings.HasPrefix(session.Outcome, ReviewSubmissionUncertainOutcome+": ") || session.Outcome != turnAction.Outcome {
		t.Fatalf("uncertain checkpoint run=%#v session=%#v turn=%#v submissions=%d", checkpoint, session, turnAction, externals.submissions)
	}

	outcome, err := service.executeReviewRun(context.Background(), job, run, externals, store)
	recovered := store.runs[run.ID]
	if err != nil || outcome.Status != "completed" || externals.submissions != 1 || !externals.boundBeforeWait || recovered.State != AgentRunCompleted || recovered.SessionID != "review-session" || recovered.NativeTurnID != "review-turn" {
		t.Fatalf("recovered run=%#v outcome=%#v submissions=%d err=%v", recovered, outcome, externals.submissions, err)
	}
}

func TestSuccessfulReviewSubmissionBindsDirectResponseBeforeWaiting(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	store := reviewRecoveryStoreFor(base, run)
	externals := &reviewRecoveryExternals{store: store}

	outcome, err := (Service{Store: store}).executeReviewRun(context.Background(), job, run, externals, store)
	persisted := store.runs[run.ID]
	if err != nil || outcome.Status != "completed" || externals.submissions != 1 || !externals.boundBeforeWait || persisted.SessionID != "review-session" || persisted.NativeTurnID != "review-turn" || persisted.State != AgentRunCompleted {
		t.Fatalf("direct binding run=%#v outcome=%#v submissions=%d boundBeforeWait=%t err=%v", persisted, outcome, externals.submissions, externals.boundBeforeWait, err)
	}
}

func TestUncertainReviewSubmissionWithNoVisibleSessionRemainsUncertain(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	store := reviewRecoveryStoreFor(base, run)
	if err := store.UncertainReviewSubmission(context.Background(), run.ID, store.sessionActionID, "response lost"); err != nil {
		t.Fatal(err)
	}
	externals := &reviewRecoveryExternals{recoverErr: reviewVisibilityTestError("strict review bound native Session is not yet visible")}

	if _, err := (Service{Store: store}).executeReviewRun(context.Background(), job, run, externals, store); err == nil || !strings.Contains(err.Error(), "not yet visible") || attentionNeeded(err) {
		t.Fatalf("empty recovery error=%v", err)
	}
	checkpoint := store.runs[run.ID]
	if externals.submissions != 0 || externals.recoveries != 1 || checkpoint.State != AgentRunUncertain || checkpoint.SessionID != "" || checkpoint.NativeTurnID != "" || store.actions[store.sessionActionID].State != ActionUncertain || store.actions[run.ActionID].State != ActionUncertain {
		t.Fatalf("empty recovery mutated checkpoint run=%#v submissions=%d", checkpoint, externals.submissions)
	}
}

func TestReviewPhaseKeepsTemporaryUncertainSubmissionRecoverable(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	job.WorkflowPhase = "reviewing"
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	recoveryStore := reviewRecoveryStoreFor(base, run)
	if err := recoveryStore.UncertainReviewSubmission(context.Background(), run.ID, recoveryStore.sessionActionID, "response lost"); err != nil {
		t.Fatal(err)
	}
	store := &reviewPhaseRecoveryStore{
		reviewRecoveryStore: recoveryStore,
		plan:                ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, State: "final", Final: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.RoleCriticalBoundary}}},
	}
	externals := &reviewRecoveryExternals{recoverErr: reviewVisibilityTestError("strict review bound native Session is not yet visible")}

	disposition, progressed, err := (Service{Store: store}).executeSelectedReviews(context.Background(), job, store, externals)
	if err == nil || !strings.Contains(err.Error(), "not yet visible") || disposition != RunIdle || progressed || base.jobs[job.ID].WorkflowPhase == "blocked" || externals.recoveries != 1 || externals.submissions != 0 {
		t.Fatalf("disposition=%s progressed=%t job=%#v recoveries=%d submissions=%d err=%v", disposition, progressed, base.jobs[job.ID], externals.recoveries, externals.submissions, err)
	}
}

func TestUncertainReviewSubmissionPositiveConflictFailsClosed(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	store := reviewRecoveryStoreFor(base, run)
	if err := store.UncertainReviewSubmission(context.Background(), run.ID, store.sessionActionID, "response lost"); err != nil {
		t.Fatal(err)
	}
	externals := &reviewRecoveryExternals{recoverErr: reviewAttentionTestError("competing native Session")}
	service := Service{Store: store}

	if _, err := service.executeReviewRun(context.Background(), job, run, externals, store); err == nil || !strings.Contains(err.Error(), "competing") {
		t.Fatalf("conflicting recovery error=%v", err)
	}
	failed := store.runs[run.ID]
	if failed.State != AgentRunFailed || externals.recoveries != 1 || externals.submissions != 0 || failed.ClaimEvidenceID != "" || failed.ObservedEvidenceID != "" {
		t.Fatalf("conflicting recovery run=%#v recoveries=%d submissions=%d", failed, externals.recoveries, externals.submissions)
	}
	if outcome, err := service.executeReviewRun(context.Background(), job, run, externals, store); err != nil || outcome.Status != string(AgentRunFailed) || externals.recoveries != 1 {
		t.Fatalf("terminal retry outcome=%#v recoveries=%d err=%v", outcome, externals.recoveries, err)
	}
}

func TestBoundUncertainReviewReentersExactReadOnlyRecoveryWithoutSubmission(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	store := reviewRecoveryStoreFor(base, run)
	controllerID := ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
	run.State, run.BaselineRecorded = AgentRunUncertain, true
	run.SessionID, run.NativeTurnID, run.ReviewerAppServer = "review-session", "review-turn", controllerID
	base.runs[run.ID] = run
	session := base.actions[store.sessionActionID]
	session.State, session.ExternalID, session.Outcome = ActionSucceeded, run.SessionID, controllerID
	base.actions[session.ID] = session
	turnAction := base.actions[run.ActionID]
	turnAction.State, turnAction.ExternalID, turnAction.Outcome = ActionSucceeded, run.NativeTurnID, "submitted"
	base.actions[turnAction.ID] = turnAction
	externals := &reviewRecoveryExternals{turn: NativeTurn{ID: run.NativeTurnID, Status: "running"}, store: store}

	outcome, err := (Service{Store: store}).executeReviewRun(context.Background(), job, run, externals, store)
	recovered := base.runs[run.ID]
	if err != nil || outcome.Status != "completed" || recovered.State != AgentRunCompleted || recovered.SessionID != run.SessionID || recovered.NativeTurnID != run.NativeTurnID || recovered.ReviewerAppServer != controllerID || externals.histories != 1 || externals.waits != 1 || externals.recoveries != 0 || externals.submissions != 0 {
		t.Fatalf("bound recovery run=%#v outcome=%#v histories=%d waits=%d recoveries=%d submissions=%d err=%v", recovered, outcome, externals.histories, externals.waits, externals.recoveries, externals.submissions, err)
	}
}

func TestUncertainReviewPartialNativeBindingFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		sessionID string
		turnID    string
	}{
		{name: "Session only", sessionID: "review-session"},
		{name: "turn only", turnID: "review-turn"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newMemoryStore()
			job := testJob()
			base.jobs[job.ID] = job
			run := preparedReviewRecoveryRun(t, base, job)
			store := reviewRecoveryStoreFor(base, run)
			run.State, run.SessionID, run.NativeTurnID = AgentRunUncertain, test.sessionID, test.turnID
			base.runs[run.ID] = run
			session := base.actions[store.sessionActionID]
			session.State = ActionSucceeded
			base.actions[session.ID] = session
			externals := &reviewRecoveryExternals{}

			_, err := (Service{Store: store}).executeReviewRun(context.Background(), job, run, externals, store)
			settled := base.runs[run.ID]
			if err == nil || !strings.Contains(err.Error(), "partial native Session/turn binding") || !attentionNeeded(err) || !strings.Contains(settled.Attention, "partial native Session/turn binding") || externals.histories != 0 || externals.waits != 0 || externals.recoveries != 0 || externals.submissions != 0 {
				t.Fatalf("partial binding run=%#v error=%v histories=%d waits=%d recoveries=%d submissions=%d", settled, err, externals.histories, externals.waits, externals.recoveries, externals.submissions)
			}
		})
	}
}

func TestBoundUncertainReviewRequiresSucceededSessionAction(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	store := reviewRecoveryStoreFor(base, run)
	run.State, run.SessionID, run.NativeTurnID = AgentRunUncertain, "review-session", "review-turn"
	base.runs[run.ID] = run
	externals := &reviewRecoveryExternals{}

	_, err := (Service{Store: store}).executeReviewRun(context.Background(), job, run, externals, store)
	settled := base.runs[run.ID]
	if err == nil || !strings.Contains(err.Error(), "succeeded Session Action") || !attentionNeeded(err) || !strings.Contains(settled.Attention, "succeeded Session Action") || externals.histories != 0 || externals.waits != 0 || externals.recoveries != 0 || externals.submissions != 0 {
		t.Fatalf("Session Action guard run=%#v error=%v histories=%d waits=%d recoveries=%d submissions=%d", settled, err, externals.histories, externals.waits, externals.recoveries, externals.submissions)
	}
}

func TestMalformedCompletedFindingBlocksWithoutPersistenceOrReadiness(t *testing.T) {
	base := newMemoryStore()
	job := testJob()
	job.WorkflowPhase = "reviewing"
	base.jobs[job.ID] = job
	run := preparedReviewRecoveryRun(t, base, job)
	now := time.Now().UTC()
	run.StartedAt, run.FinishedAt = now.Add(-time.Second), now
	base.runs[run.ID] = run
	recoveryStore := reviewRecoveryStoreFor(base, run)
	store := &reviewPhaseRecoveryStore{
		reviewRecoveryStore: recoveryStore,
		plan:                ReviewPlanRecord{JobID: job.ID, Revision: job.Revision, State: "final", Final: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.RoleCriticalBoundary}}},
	}
	externals := &reviewRecoveryExternals{output: `{"summary":"clear","rationale":"no issue"}`}
	service := Service{Store: store, Externals: externals, Evidence: evidence.Store{Root: t.TempDir()}}

	_, _, err := service.advanceReview(context.Background(), job)
	settled := base.jobs[job.ID]
	if err == nil || settled.WorkflowPhase != "blocked" || recoveryStore.recordedResults != 0 || recoveryStore.readyCalls != 0 {
		t.Fatalf("malformed finding job=%#v recorded=%d ready=%d err=%v", settled, recoveryStore.recordedResults, recoveryStore.readyCalls, err)
	}
}

func TestTriageRequiredRolesGatePersistenceAndExplicitEmptyMeansNoReview(t *testing.T) {
	for _, test := range []struct {
		name         string
		output       string
		wantBlocked  bool
		wantRecorded int
		wantNoReview bool
	}{
		{name: "omitted roles blocks", output: `{"rationale":"no specialized review needed"}`, wantBlocked: true},
		{name: "null roles blocks", output: `{"roles":null,"rationale":"no specialized review needed"}`, wantBlocked: true},
		{name: "explicit empty roles is intentional no review", output: `{"roles":[],"rationale":"no specialized review needed"}`, wantRecorded: 1, wantNoReview: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newMemoryStore()
			job := testJob()
			job.WorkflowPhase = "review-triage"
			base.jobs[job.ID] = job
			run := preparedReviewRecoveryRunWithRole(t, base, job, ReviewTriageRole)
			now := time.Now().UTC()
			run.StartedAt, run.FinishedAt = now.Add(-time.Second), now
			base.runs[run.ID] = run
			recoveryStore := reviewRecoveryStoreFor(base, run)
			store := &reviewPhaseRecoveryStore{
				reviewRecoveryStore: recoveryStore,
				plan: ReviewPlanRecord{
					JobID: job.ID, Revision: job.Revision, State: "triage-pending", TriageRunID: run.ID,
					Initial: policy.ReviewPlan{Decision: "triage", NeedsTriage: true},
				},
			}
			externals := &reviewRecoveryExternals{output: test.output}
			service := Service{Store: store, Externals: externals, Evidence: evidence.Store{Root: t.TempDir()}}

			disposition, progressed, err := service.advanceReview(context.Background(), job)
			settled := base.jobs[job.ID]
			blocked := settled.WorkflowPhase == "blocked"
			if err != nil || blocked != test.wantBlocked || recoveryStore.recordedTriage != test.wantRecorded || recoveryStore.readyCalls != 0 {
				t.Fatalf("disposition=%s progressed=%t job=%#v recorded=%d ready=%d plan=%#v err=%v", disposition, progressed, settled, recoveryStore.recordedTriage, recoveryStore.readyCalls, recoveryStore.triagePlan, err)
			}
			if test.wantBlocked && (disposition != RunBlocked || progressed) {
				t.Fatalf("malformed triage disposition=%s progressed=%t", disposition, progressed)
			}
			if test.wantNoReview && (!progressed || recoveryStore.triagePlan.Decision != "no-review" || recoveryStore.triagePlan.NeedsTriage || len(recoveryStore.triagePlan.Roles) != 0 || settled.WorkflowPhase != "ready") {
				t.Fatalf("explicit empty triage did not settle no-review: disposition=%s progressed=%t plan=%#v job=%#v", disposition, progressed, recoveryStore.triagePlan, settled)
			}
		})
	}
}

func preparedReviewRecoveryRun(t *testing.T, base *memoryStore, job Job) AgentRun {
	return preparedReviewRecoveryRunWithRole(t, base, job, string(policy.RoleCriticalBoundary))
}

func preparedReviewRecoveryRunWithRole(t *testing.T, base *memoryStore, job Job, role string) AgentRun {
	t.Helper()
	outputContract := policy.FindingOutputContract
	if role == ReviewTriageRole {
		outputContract = policy.TriageOutputContract
	}
	run := AgentRun{ID: ReviewAgentRunID(job.ID, job.Revision, role), JobID: job.ID, Revision: job.Revision, Role: role, Capability: ReviewReadOnlyCapability, Workspace: "/workspace/job", InputContract: "bounded", OutputContract: outputContract, State: AgentRunPending, ReviewerSandboxState: "created", ReviewerRouteState: "active", CheckoutState: "verified", ReviewerOwnerNonce: strings.Repeat("2", 64), SubmissionNonce: strings.Repeat("1", 64), InputDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("bounded")))}
	run.ReviewerSandboxID = ReviewSandboxName(run.ID)
	run.ActionID = ScopedActionID(job.ID, ActionTurnStart, run.ID)
	base.runs[run.ID] = run
	for _, kind := range []ActionKind{ActionSandboxCreate, ActionRouteCreate, ActionReviewWorkspaceCreate, ActionSessionStart, ActionTurnStart} {
		id := ScopedActionID(job.ID, kind, run.ID)
		state := ActionSucceeded
		if kind == ActionSessionStart || kind == ActionTurnStart {
			state = ActionPending
		}
		base.actions[id] = Action{ID: id, JobID: job.ID, Kind: kind, Scope: run.ID, State: state}
	}
	return run
}

func reviewRecoveryStoreFor(base *memoryStore, run AgentRun) *reviewRecoveryStore {
	return &reviewRecoveryStore{
		codingMemoryStore: newCodingMemoryStore(base),
		runID:             run.ID,
		sandboxActionID:   ScopedActionID(run.JobID, ActionSandboxCreate, run.ID),
		routeActionID:     ScopedActionID(run.JobID, ActionRouteCreate, run.ID),
		workspaceActionID: ScopedActionID(run.JobID, ActionReviewWorkspaceCreate, run.ID),
		sessionActionID:   ScopedActionID(run.JobID, ActionSessionStart, run.ID),
	}
}

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
			store := &reviewRecoveryStore{codingMemoryStore: newCodingMemoryStore(base), runID: run.ID, sandboxActionID: actions[ActionSandboxCreate].ID, routeActionID: actions[ActionRouteCreate].ID, workspaceActionID: actions[ActionReviewWorkspaceCreate].ID, sessionActionID: actions[ActionSessionStart].ID}
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
	*codingMemoryStore
	runID, sandboxActionID, routeActionID, workspaceActionID, sessionActionID string
	recordedResults, recordedTriage, readyCalls                               int
	triagePlan                                                                policy.ReviewPlan
}

func newCodingMemoryStore(base *memoryStore) *codingMemoryStore {
	return &codingMemoryStore{memoryStore: base, checks: map[string]Check{}, evidence: map[string]Evidence{}}
}

type reviewPhaseRecoveryStore struct {
	*reviewRecoveryStore
	plan ReviewPlanRecord
}

func (s *reviewPhaseRecoveryStore) ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error) {
	return s.plan, nil
}

func (s *reviewPhaseRecoveryStore) ReviewRuns(context.Context, string, string) ([]ReviewRunView, error) {
	return []ReviewRunView{{AgentRun: s.runs[s.runID]}}, nil
}

func (s *reviewRecoveryStore) CompleteAction(_ context.Context, id string, receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[id]
	action.State, action.ExternalID, action.Outcome = ActionSucceeded, receipt.ExternalID, receipt.Outcome
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
func (s *reviewRecoveryStore) RecordTriageResult(_ context.Context, runID string, _ NativeTurn, _, _ Evidence, final policy.ReviewPlan, _ string) error {
	s.recordedTriage++
	s.triagePlan = final
	if final.Decision == "no-review" {
		s.mu.Lock()
		run := s.runs[runID]
		job := s.jobs[run.JobID]
		job.WorkflowPhase = "ready"
		s.jobs[run.JobID] = job
		s.mu.Unlock()
	}
	return nil
}
func (s *reviewRecoveryStore) AdmitReviewRepair(context.Context, string, string) (Message, bool, error) {
	return Message{}, false, nil
}
func (s *reviewRecoveryStore) MarkReviewReady(context.Context, string, string) error {
	s.readyCalls++
	return nil
}
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
	*fakeExternals
	mu               sync.Mutex
	submissions      int
	turn             NativeTurn
	initialErr       error
	lostResponseOnce bool
	recoverErr       error
	recoveries       int
	histories        int
	waits            int
	controllerID     string
	store            *reviewRecoveryStore
	boundBeforeWait  bool
	output           string
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
	if e.lostResponseOnce {
		e.lostResponseOnce = false
		return ReviewNativeBinding{}, errors.New("review turn submission response was lost")
	}
	return e.reviewBinding(run), nil
}
func (e *reviewRecoveryExternals) ReviewRecover(_ context.Context, _ Job, run AgentRun) (ReviewNativeBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recoveries++
	if e.recoverErr != nil {
		return ReviewNativeBinding{}, e.recoverErr
	}
	if e.turn.ID == "" {
		return ReviewNativeBinding{}, reviewVisibilityTestError("strict review bound native Session is not yet visible")
	}
	return e.reviewBinding(run), nil
}
func (e *reviewRecoveryExternals) ReviewTurns(_ context.Context, _ Job, run AgentRun) (ReviewNativeHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.histories++
	return e.reviewHistory(run), nil
}
func (e *reviewRecoveryExternals) ReviewWait(_ context.Context, _ Job, run AgentRun, _ string) (ReviewNativeBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.waits++
	if e.store != nil {
		e.store.mu.Lock()
		persisted := e.store.runs[run.ID]
		e.store.mu.Unlock()
		e.boundBeforeWait = persisted.SessionID == "review-session" && persisted.NativeTurnID == "review-turn" && persisted.State == AgentRunActive
	}
	e.turn.Status = "completed"
	e.turn.Output = e.output
	if e.turn.Output == "" {
		e.turn.Output = `{"material":false,"summary":"clear","rationale":"clear","affected_roles":[],"affected_checks":[]}`
	}
	return e.reviewBinding(run), nil
}
