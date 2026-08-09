package spine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
	policy "github.com/aphronio/dorf/internal/review"
)

func TestRunReviewRecordsRawFeedbackAsAnAgentRunMessage(t *testing.T) {
	store, externals, job, run := reviewFixture(t)
	externals.output = "Potential issue: the retry path may retain a stale lease. Please verify the lease-release Check. {ordinary prose, not a protocol}"
	service := Service{Store: store, Externals: externals, Evidence: evidence.Store{Root: t.TempDir()}}

	if err := service.RunReview(context.Background(), job, run.ID); err != nil {
		t.Fatal(err)
	}
	if externals.submissions != 1 || externals.recoveries != 0 || store.feedbackCalls != 1 {
		t.Fatalf("submissions=%d recoveries=%d feedback calls=%d", externals.submissions, externals.recoveries, store.feedbackCalls)
	}
	wantMessage := Message{
		ID:       MessageID(job.ID, MessageFromAgent, run.ID),
		JobID:    job.ID,
		FromKind: MessageFromAgent,
		FromID:   run.ID,
		Sequence: 1,
		Input:    externals.output,
		Intent:   MessageFollow,
	}
	if store.feedback != wantMessage {
		t.Fatalf("feedback Message=%#v want=%#v", store.feedback, wantMessage)
	}
	if store.claim.Kind != "review-feedback" || store.claim.MediaType != "text/plain; charset=utf-8" || store.claim.Provenance != "claim" {
		t.Fatalf("claim Evidence=%#v", store.claim)
	}
	contents, err := service.Evidence.ReadVerified(store.claim.Digest, store.claim.ByteSize)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != externals.output {
		t.Fatalf("retained feedback=%q want exact raw output %q", contents, externals.output)
	}

	view, err := store.ReviewRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	view.FeedbackMessageID = store.feedback.ID
	verification := VerifyReviewRunEvidence(view, []Evidence{store.claim, store.observed}, service.Evidence)
	if !verification.Verified {
		t.Fatalf("review Evidence did not verify: %#v", verification)
	}
	view.Capability = "workspace-write"
	if verification := VerifyReviewRunEvidence(view, []Evidence{store.claim, store.observed}, service.Evidence); verification.Verified || !strings.Contains(verification.Error, "least-capability") {
		t.Fatalf("mutable reviewer capability verified: %#v", verification)
	}
}

func TestRunReviewUsesSharedNativeRecoveryWithoutResubmission(t *testing.T) {
	store, externals, job, run := reviewFixture(t)
	run.BaselineRecorded = true
	run.State = AgentRunSubmitting
	store.runs[run.ID] = run.AgentRun
	externals.output = "No material issue found in the bounded critical path."
	service := Service{Store: store, Externals: externals, Evidence: evidence.Store{Root: t.TempDir()}}

	if err := service.RunReview(context.Background(), job, run.ID); err != nil {
		t.Fatal(err)
	}
	if externals.submissions != 0 || externals.recoveries != 1 || store.feedback.Input != externals.output {
		t.Fatalf("submissions=%d recoveries=%d feedback=%#v", externals.submissions, externals.recoveries, store.feedback)
	}
	settled := store.runs[run.ID]
	if settled.State != AgentRunCompleted || settled.NativeTurnID != externals.turnID || settled.SessionID != externals.sessionID {
		t.Fatalf("recovered AgentRun=%#v", settled)
	}
}

func TestReviewerNativeOwnershipIsExact(t *testing.T) {
	_, _, _, run := reviewFixture(t)
	run.SessionID = "session-review"
	run.ReviewerAppServer = ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
	if err := validateReviewNativeOwner(run, run.ReviewerAppServer, run.SessionID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, controller, session string
	}{
		{name: "foreign controller", controller: ReviewControllerID("foreign-run", run.ReviewerSandboxID, run.ReviewerOwnerNonce), session: run.SessionID},
		{name: "foreign session", controller: run.ReviewerAppServer, session: "foreign-session"},
		{name: "missing owner", session: run.SessionID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateReviewNativeOwner(run, test.controller, test.session); err == nil || !attentionNeeded(err) {
				t.Fatalf("ownership error=%v", err)
			}
		})
	}
}

func TestReviewerCleanupReclaimsExactResourcesOnce(t *testing.T) {
	store, externals, job, run := reviewFixture(t)
	job.AdmissionOpen = false
	job.CleanupState = CleanupScheduled
	store.jobs[job.ID] = job
	run.State = AgentRunActive
	store.runs[run.ID] = run.AgentRun
	store.cleanupRuns = []ReviewRunView{run}
	for _, kind := range []ActionKind{ActionRouteRevoke, ActionSandboxDelete} {
		id := ScopedActionID(job.ID, kind, run.ID)
		store.actions[id] = Action{ID: id, JobID: job.ID, Kind: kind, Scope: run.ID, State: ActionPending}
	}
	service := Service{Store: store, Externals: externals}

	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if want := []ActionKind{ActionRouteRevoke, ActionSandboxDelete}; !reflect.DeepEqual(externals.cleanupEffects, want) {
		t.Fatalf("cleanup effects=%v want=%v", externals.cleanupEffects, want)
	}
	if store.runs[run.ID].State != AgentRunInterrupted || store.jobs[job.ID].CleanupState != CleanupComplete {
		t.Fatalf("run=%#v job=%#v", store.runs[run.ID], store.jobs[job.ID])
	}
}

type reviewTestStore struct {
	*codingMemoryStore
	feedback      Message
	claim         Evidence
	observed      Evidence
	feedbackCalls int
	cleanupRuns   []ReviewRunView
	projections   map[string]ReviewRunProjection
}

func newReviewTestStore() *reviewTestStore {
	base := newMemoryStore()
	return &reviewTestStore{codingMemoryStore: &codingMemoryStore{memoryStore: base, checks: map[string]Check{}, evidence: map[string]Evidence{}}, projections: map[string]ReviewRunProjection{}}
}

func (s *reviewTestStore) CompleteAction(_ context.Context, id string, receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action, ok := s.actions[id]
	if !ok {
		return fmt.Errorf("unknown Action %s", id)
	}
	action.State, action.ExternalID, action.Outcome = ActionSucceeded, receipt.ExternalID, receipt.Outcome
	s.actions[id] = action
	if action.Scope == "" {
		return nil
	}
	run := s.runs[action.Scope]
	projection := s.projections[action.Scope]
	switch action.Kind {
	case ActionSessionStart:
		run.SessionID, projection.ReviewerAppServer = receipt.ExternalID, receipt.Outcome
	case ActionRouteRevoke:
		projection.ReviewerRouteState = "revoked"
	case ActionSandboxDelete:
		projection.ReviewerSandboxState = "deleted"
	}
	s.runs[run.ID] = run
	s.projections[run.ID] = projection
	return nil
}

func (s *reviewTestStore) MarkChecksVerified(context.Context, string, string, []string) error {
	return nil
}
func (s *reviewTestStore) ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error) {
	return ReviewPlanRecord{}, nil
}
func (s *reviewTestStore) RecordReviewPolicy(context.Context, ReviewPlanRecord) error { return nil }
func (s *reviewTestStore) ReviewRuns(context.Context, string, string) ([]ReviewRunView, error) {
	return nil, nil
}
func (s *reviewTestStore) AllReviewRuns(context.Context, string) ([]ReviewRunView, error) {
	return nil, nil
}
func (s *reviewTestStore) CleanupReviewRuns(context.Context, string) ([]ReviewRunView, error) {
	return append([]ReviewRunView(nil), s.cleanupRuns...), nil
}
func (s *reviewTestStore) reviewAction(runID string, kind ActionKind) (Action, error) {
	action, ok := s.actions[ScopedActionID(s.runs[runID].JobID, kind, runID)]
	if !ok {
		return Action{}, fmt.Errorf("missing %s Action", kind)
	}
	return action, nil
}
func (s *reviewTestStore) BeginReviewSandbox(_ context.Context, runID string) (Action, error) {
	return s.reviewAction(runID, ActionSandboxCreate)
}
func (s *reviewTestStore) BeginReviewRoute(_ context.Context, runID string) (Action, error) {
	return s.reviewAction(runID, ActionRouteCreate)
}
func (s *reviewTestStore) BeginReviewWorkspace(_ context.Context, runID string) (Action, error) {
	return s.reviewAction(runID, ActionReviewWorkspaceCreate)
}
func (s *reviewTestStore) BeginReviewSession(_ context.Context, runID string) (Action, error) {
	return s.reviewAction(runID, ActionSessionStart)
}
func (s *reviewTestStore) ReviewRun(_ context.Context, runID string) (ReviewRunView, error) {
	run, ok := s.runs[runID]
	if !ok {
		return ReviewRunView{}, errors.New("missing review AgentRun")
	}
	return ReviewRunView{AgentRun: run, ReviewRunProjection: s.projections[runID]}, nil
}
func (s *reviewTestStore) RecordReviewFeedback(_ context.Context, runID string, outcome NativeTurn, claim, observed Evidence) (Message, bool, error) {
	s.feedbackCalls++
	s.claim, s.observed = claim, observed
	run := s.runs[runID]
	projection := s.projections[runID]
	projection.ClaimEvidenceID, projection.ObservedEvidenceID = claim.ID, observed.ID
	s.projections[runID] = projection
	s.feedback = Message{ID: MessageID(run.JobID, MessageFromAgent, runID), JobID: run.JobID, FromKind: MessageFromAgent, FromID: runID, Sequence: 1, Input: outcome.Output, Intent: MessageFollow}
	return s.feedback, true, nil
}
func (s *reviewTestStore) CompleteReviewFeedback(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (s *reviewTestStore) BeginReviewRouteCleanup(_ context.Context, runID string) (Action, error) {
	return s.reviewAction(runID, ActionRouteRevoke)
}
func (s *reviewTestStore) BeginReviewSandboxCleanup(_ context.Context, runID string) (Action, error) {
	return s.reviewAction(runID, ActionSandboxDelete)
}
func (s *reviewTestStore) InterruptReviewRun(_ context.Context, runID, reason string) error {
	run := s.runs[runID]
	run.State, run.Attention = AgentRunInterrupted, reason
	s.runs[runID] = run
	return nil
}
func (s *reviewTestStore) RecordReviewPostState(_ context.Context, runID string, _ Receipt) error {
	projection := s.projections[runID]
	projection.PostReviewState = "verified"
	s.projections[runID] = projection
	return nil
}

type reviewTestExternals struct {
	*fakeExternals
	controllerID   string
	sessionID      string
	turnID         string
	output         string
	submissions    int
	recoveries     int
	cleanupEffects []ActionKind
}

func (e *reviewTestExternals) RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error) {
	return policy.ChangeFacts{}, nil
}
func (e *reviewTestExternals) ReviewSandboxCreate(context.Context, Job, ReviewRunView, Action) (Receipt, error) {
	return Receipt{}, errors.New("unexpected reviewer Sandbox create")
}
func (e *reviewTestExternals) ReviewRouteCreate(context.Context, Job, ReviewRunView, Action) (Receipt, error) {
	return Receipt{}, errors.New("unexpected reviewer route create")
}
func (e *reviewTestExternals) ReviewWorkspaceCreate(context.Context, Job, ReviewRunView, Action) (Receipt, error) {
	return Receipt{}, errors.New("unexpected reviewer workspace create")
}
func (e *reviewTestExternals) ReviewWorkspaceVerify(context.Context, Job, ReviewRunView) (Receipt, error) {
	return Receipt{ExternalID: "/workspace/job", Outcome: "verified"}, nil
}
func (e *reviewTestExternals) ReviewRouteRevoke(_ context.Context, _ Job, _ ReviewRunView, _ Action) (Receipt, error) {
	e.cleanupEffects = append(e.cleanupEffects, ActionRouteRevoke)
	return Receipt{}, nil
}
func (e *reviewTestExternals) ReviewSandboxDelete(_ context.Context, _ Job, _ ReviewRunView, _ Action) (Receipt, error) {
	e.cleanupEffects = append(e.cleanupEffects, ActionSandboxDelete)
	return Receipt{}, nil
}
func (e *reviewTestExternals) binding() ReviewNativeBinding {
	return ReviewNativeBinding{AppServerID: e.controllerID, SessionID: e.sessionID, Turn: NativeTurn{ID: e.turnID, Status: "completed", Output: e.output}}
}
func (e *reviewTestExternals) ReviewInitialTurn(context.Context, Job, ReviewRunView) (ReviewNativeBinding, error) {
	e.submissions++
	return e.binding(), nil
}
func (e *reviewTestExternals) ReviewRecover(context.Context, Job, ReviewRunView) (ReviewNativeBinding, error) {
	e.recoveries++
	return e.binding(), nil
}
func (e *reviewTestExternals) ReviewTurns(context.Context, Job, ReviewRunView) (ReviewNativeHistory, error) {
	binding := e.binding()
	return ReviewNativeHistory{AppServerID: binding.AppServerID, SessionID: binding.SessionID, Turns: []NativeTurn{binding.Turn}}, nil
}
func (e *reviewTestExternals) ReviewWait(context.Context, Job, ReviewRunView, string) (ReviewNativeBinding, error) {
	return e.binding(), nil
}

func reviewFixture(t *testing.T) (*reviewTestStore, *reviewTestExternals, Job, ReviewRunView) {
	t.Helper()
	store := newReviewTestStore()
	job := testJob()
	job.WorkflowPhase = "reviewing"
	store.jobs[job.ID] = job
	input := "bounded reviewer input"
	runID := ReviewAgentRunID(job.ID, job.Revision, string(policy.RoleCriticalBoundary))
	now := time.Now().UTC().Truncate(time.Microsecond)
	run := ReviewRunView{
		AgentRun: AgentRun{
			ID: runID, JobID: job.ID, ActionID: ScopedActionID(job.ID, ActionTurnStart, runID),
			Revision: job.Revision, Role: string(policy.RoleCriticalBoundary), State: AgentRunPending,
			Capability: ReviewReadOnlyCapability, Workspace: "/workspace/job", InputContract: input,
			StartedAt: now, FinishedAt: now.Add(time.Second),
		},
		ReviewRunProjection: ReviewRunProjection{
			ReviewerSandboxID: ReviewSandboxName(runID), ReviewerRouteID: "route-review",
			ReviewerOwnerNonce: strings.Repeat("a", 64), SubmissionNonce: strings.Repeat("b", 64),
			InputDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(input))), RevisionTree: strings.Repeat("c", 40),
			ReviewerSandboxState: "created", ReviewerRouteState: "active", CheckoutState: "verified",
		},
	}
	store.runs[run.ID] = run.AgentRun
	store.projections[run.ID] = run.ReviewRunProjection
	for _, item := range []struct {
		kind  ActionKind
		state ActionState
	}{
		{ActionSandboxCreate, ActionSucceeded},
		{ActionReviewWorkspaceCreate, ActionSucceeded},
		{ActionRouteCreate, ActionSucceeded},
		{ActionSessionStart, ActionPending},
		{ActionTurnStart, ActionPending},
	} {
		id := ScopedActionID(job.ID, item.kind, run.ID)
		store.actions[id] = Action{ID: id, JobID: job.ID, Kind: item.kind, Scope: run.ID, State: item.state}
	}
	externals := &reviewTestExternals{
		fakeExternals: newFakeExternals(), controllerID: ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce),
		sessionID: "session-review", turnID: "turn-review",
	}
	return store, externals, job, run
}

var _ ReviewStore = (*reviewTestStore)(nil)
var _ ReviewExternals = (*reviewTestExternals)(nil)
