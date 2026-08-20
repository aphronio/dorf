package workflow

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

// WorkKind is the next concrete operation in Dorf's coding workflow. It is a
// disposable answer derived from product facts, never a value stored on Job.
type WorkKind string

const (
	WorkComplete        WorkKind = "complete"
	WorkAttention       WorkKind = "attention"
	WorkAction          WorkKind = "action"
	WorkSetupRepository WorkKind = "setup-repository"
	WorkRunReviewer     WorkKind = "run-reviewer"
	WorkDeliverMessage  WorkKind = "deliver-message"
	WorkObserveAgent    WorkKind = "observe-agent-run"
	WorkObserveRevision WorkKind = "observe-revision"
	WorkRunChecks       WorkKind = "run-checks"
	WorkChooseReview    WorkKind = "choose-review"
	WorkPublishProposal WorkKind = "publish-proposal"
	WorkObserveProposal WorkKind = "observe-proposal"
)

// Work names the natural fact which owns the next operation. FactID is an
// Action, AgentRun, Check, Revision, Proposal, or Outcome identity as
// appropriate. Scope names the concrete resource only when an Action needs
// one. Work is useful for explanation and exact execution, but deliberately
// not durable.
type Work struct {
	Kind       WorkKind         `json:"kind"`
	Revision   string           `json:"revision,omitempty"`
	FactID     string           `json:"fact_id,omitempty"`
	ActionKind spine.ActionKind `json:"action,omitempty"`
	Scope      string           `json:"scope,omitempty"`
	Detail     string           `json:"detail,omitempty"`
}

func (w Work) Description() string {
	definition := CodingToProposalDefinition()
	if w.Kind == WorkAction {
		return definition.ActionLabel(w.ActionKind)
	}
	if w.Kind == WorkObserveAgent {
		return definition.AgentRoleLabel("implement") + " running"
	}
	if w.Kind == WorkDeliverMessage {
		return "Delivering Message to " + lowerFirst(definition.AgentRoleLabel("implement"))
	}
	if w.Kind == WorkRunReviewer {
		return "Reviewer running"
	}
	return definition.OperationLabel(string(w.Kind), humanizeIdentifier(string(w.Kind)))
}

// Snapshot is Dorf's concrete, coding-specific read model. It is loaded once
// for inspection or one RunJob decision, then projected without another
// database read. The reads are not a database transaction; every operation
// still revalidates its owning fact before recording an effect.
type Snapshot struct {
	Job            spine.CodingJob
	Sandboxes      []spine.Sandbox
	MainSandbox    spine.Sandbox
	Actions        []spine.Action
	SelectedSetup  *spine.Action
	Delivery       *spine.Delivery
	Deliveries     []spine.Delivery
	Revisions      []spine.Revision
	DeclaredChecks []spine.DeclaredCheck
	Checks         []spine.Check
	Evidence       []spine.Evidence
	ReviewPlans    []spine.ReviewPlanRecord
	Proposal       *spine.GitHubProposal
	Outcome        *spine.JobOutcome
}

// Projection is a disposable explanation derived from one Snapshot. It is
// shared by execution and inspection and is never persisted.
type Projection struct {
	CurrentWork Work                      `json:"current_work"`
	Readiness   spine.ReadinessAssessment `json:"readiness"`
}

// Project derives readiness and the one next coding operation once. Once
// publication starts, its admitted input boundary is retained so a later
// accepted Message cannot strand reconciliation.
func (s Snapshot) Project(evidenceStore blob.Store) (Projection, error) {
	deliveries := spine.PublicationDeliveries(s.Deliveries, publicationIntentAt(s.Actions, s.Job.Revision))
	reviewRuns, err := s.currentReviewRuns()
	if err != nil {
		return Projection{}, err
	}
	readiness := spine.AssessReviewReadiness(
		s.Job, s.DeclaredChecks, s.Checks, s.Evidence, evidenceStore,
		s.currentReviewPlan(), reviewRuns, deliveries,
	)
	work := decideCurrentWorkWithReviewRuns(s, reviewRuns)
	if work.Kind == WorkPublishProposal && !readiness.Ready {
		work.Kind = WorkAttention
		work.Detail = "publication lost exact-Revision readiness: " + readiness.Reason
	}
	return Projection{CurrentWork: work, Readiness: readiness}, nil
}

func publicationIntentAt(actions []spine.Action, revision string) time.Time {
	for _, action := range actions {
		if action.Kind == spine.ActionGitHubPullRequest && action.Scope == revision {
			return action.CreatedAt
		}
	}
	return time.Time{}
}

// LoadSnapshot performs one staged load of the coding product facts. Check
// contracts are intentionally loaded only after repository setup succeeds.
func LoadSnapshot(ctx context.Context, store postgres.Store, jobID string) (Snapshot, error) {
	var snapshot Snapshot
	var err error
	snapshot.Job, err = store.CodingJob(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Sandboxes, err = store.Sandboxes(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	for _, sandbox := range snapshot.Sandboxes {
		if sandbox.ID == spine.MainSandboxName(jobID) {
			snapshot.MainSandbox = sandbox
			break
		}
	}
	if snapshot.MainSandbox.ID == "" {
		return snapshot, fmt.Errorf("main Sandbox %s: %w", spine.MainSandboxName(jobID), postgres.ErrNotFound)
	}
	snapshot.Actions, err = store.Actions(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.SelectedSetup, err = store.SelectedSetup(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Deliveries, err = store.Deliveries(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Revisions, err = store.Revisions(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Evidence, err = store.Evidence(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.ReviewPlans, err = store.ReviewPlans(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Proposal, err = store.Proposal(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Outcome, err = store.Outcome(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	if !codingPrerequisitesComplete(snapshot) {
		return snapshot, nil
	}
	snapshot.DeclaredChecks, err = store.DeclaredChecks(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Checks, err = store.Checks(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Outcome == nil && snapshot.Job.AdmissionOpen {
		snapshot.Delivery, err = store.DeliveryCandidate(ctx, jobID)
		if err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func (s Snapshot) currentReviewPlan() *spine.ReviewPlanRecord {
	for i := range s.ReviewPlans {
		if s.ReviewPlans[i].Revision == s.Job.Revision {
			return &s.ReviewPlans[i]
		}
	}
	return nil
}

func (s Snapshot) currentReviewRuns() ([]spine.ReviewRunView, error) {
	all, err := spine.ReviewRuns(s.Deliveries, s.Sandboxes)
	if err != nil {
		return nil, err
	}
	runs := make([]spine.ReviewRunView, 0, len(all))
	for _, run := range all {
		if run.InputRevision == s.Job.Revision {
			runs = append(runs, run)
		}
	}
	return runs, nil
}

func codingPrerequisitesComplete(f Snapshot) bool {
	return actionSucceeded(f.Actions, spine.ActionSandboxCreate, f.MainSandbox.ID) &&
		actionSucceeded(f.Actions, spine.ActionRepositoryClone, f.MainSandbox.ID) &&
		f.SelectedSetup != nil && f.SelectedSetup.State == spine.ActionSucceeded &&
		actionSucceeded(f.Actions, spine.ActionRouteCreate, f.MainSandbox.ID)
}

// decideCurrentWork is intentionally an ordinary, coding-specific decision.
// Its order is the dependency chain; do not replace it with a generic graph,
// registry, persisted projection, or database-side workflow interpreter.
func decideCurrentWork(f Snapshot) Work {
	reviewRuns, err := f.currentReviewRuns()
	if err != nil {
		return Work{Kind: WorkAttention, Revision: f.Job.Revision, FactID: f.Job.Revision, Detail: err.Error()}
	}
	return decideCurrentWorkWithReviewRuns(f, reviewRuns)
}

func decideCurrentWorkWithReviewRuns(f Snapshot, reviewRuns []spine.ReviewRunView) Work {
	work := func(kind WorkKind, factID, detail string) Work {
		return Work{Kind: kind, Revision: f.Job.Revision, FactID: factID, Detail: detail}
	}
	actionWork := func(kind spine.ActionKind, scope, detail string) Work {
		return Work{
			Kind: WorkAction, Revision: f.Job.Revision,
			FactID:     spine.ScopedActionID(f.Job.ID, kind, scope),
			ActionKind: kind, Scope: scope, Detail: detail,
		}
	}
	if f.Outcome != nil {
		return work(WorkComplete, f.Outcome.JobID, string(f.Outcome.Kind))
	}
	if !f.Job.AdmissionOpen {
		return work(WorkComplete, f.Job.ID, "admission closed")
	}

	// Infrastructure is a fixed prerequisite chain owned by the Job's main
	// Sandbox. Missing Actions are created only when RunJob executes this work.
	if !actionSucceeded(f.Actions, spine.ActionSandboxCreate, f.MainSandbox.ID) {
		return actionWork(spine.ActionSandboxCreate, f.MainSandbox.ID, "")
	}
	if !actionSucceeded(f.Actions, spine.ActionRepositoryClone, f.MainSandbox.ID) {
		return actionWork(spine.ActionRepositoryClone, f.MainSandbox.ID, "")
	}
	if f.SelectedSetup == nil {
		return work(WorkSetupRepository, spine.ActionID(f.Job.ID, spine.ActionRepositorySetup), "")
	}
	if f.SelectedSetup.State == spine.ActionFailed {
		return work(WorkAttention, f.SelectedSetup.ID, attentionDetail(f.Job, f.SelectedSetup.ID, "repository setup failed"))
	}
	if f.SelectedSetup.State != spine.ActionSucceeded {
		return work(WorkSetupRepository, f.SelectedSetup.ID, attentionDetail(f.Job, f.SelectedSetup.ID, ""))
	}
	if !actionSucceeded(f.Actions, spine.ActionRouteCreate, f.MainSandbox.ID) {
		return actionWork(spine.ActionRouteCreate, f.MainSandbox.ID, "")
	}
	// Once exact-Revision publication Actions exist, reconcile them before a
	// later accepted Message can advance the Revision. The Proposal then makes
	// that old publication unambiguous and becomes stale after the new Revision.
	if f.Proposal == nil && publicationPending(f.Actions, f.Job.Revision) {
		return work(WorkPublishProposal, f.Job.Revision, publicationDetail(f, "reconcile started publication"))
	}

	// Once ReviewPolicy selected exact reviewers, complete their independent
	// AgentRuns before consuming the feedback Messages they produce.
	plan := f.currentReviewPlan()
	if plan != nil {
		byRole := make(map[string]spine.ReviewRunView, len(reviewRuns))
		for _, run := range reviewRuns {
			byRole[run.Role] = run
		}
		for _, role := range plan.Plan.Roles {
			run, ok := byRole[string(role)]
			if !ok {
				return work(WorkAttention, f.Job.Revision, fmt.Sprintf("selected reviewer %s has no AgentRun", role))
			}
			if reviewFeedbackReturned(f.Deliveries, f.Job.ID, run.ID) {
				continue
			}
			if run.State == spine.AgentRunFailed || run.State == spine.AgentRunInterrupted || run.State == spine.AgentRunUncertain {
				return work(WorkAttention, run.ID, attentionDetail(f.Job, run.ID, agentRunAttention(run.AgentRun)))
			}
			if run.Sandbox.ID == "" || run.Sandbox.ID != run.SandboxID || run.Sandbox.JobID != f.Job.ID {
				return work(WorkAttention, run.ID, fmt.Sprintf("selected reviewer %s has no exact Job-owned Sandbox", role))
			}
			if !actionSucceeded(f.Actions, spine.ActionSandboxCreate, run.Sandbox.ID) {
				return actionWork(spine.ActionSandboxCreate, run.Sandbox.ID, string(role))
			}
			if !actionSucceeded(f.Actions, spine.ActionReviewCheckout, run.Sandbox.ID) {
				return actionWork(spine.ActionReviewCheckout, run.Sandbox.ID, string(role))
			}
			if !actionSucceeded(f.Actions, spine.ActionRouteCreate, run.Sandbox.ID) {
				return actionWork(spine.ActionRouteCreate, run.Sandbox.ID, string(role))
			}
			return work(WorkRunReviewer, run.ID, string(role))
		}
	}

	// Messages share one implementation lane: steers may target the active
	// Turn; turn-starting follows remain FIFO. DeliveryCandidate is read-only.
	if f.Delivery != nil {
		run := f.Delivery.AgentRun
		if run.State == spine.AgentRunFailed || run.State == spine.AgentRunInterrupted || run.State == spine.AgentRunUncertain {
			return work(WorkAttention, run.ID, attentionDetail(f.Job, run.ID, agentRunAttention(run)))
		}
		return work(WorkDeliverMessage, run.ID, fmt.Sprintf("Message %d", f.Delivery.Message.Sequence))
	}
	latestInput, latestTurnStart := latestImplementationRuns(f)
	if latestInput != nil && latestInput.State != spine.AgentRunCompleted {
		if latestTurnStart != nil && latestInput.ID == latestTurnStart.ID && latestInput.State == spine.AgentRunActive {
			// The active turn-starting input is observed below. A later unsettled
			// steer remains attention unless it is selected for delivery above.
		} else {
			// A failed steer may have no Turn identity when its terminal-target
			// fallback failed before binding. It is still the latest accepted input
			// and must not be skipped in favor of an older observed Turn.
			return work(WorkAttention, latestInput.ID, attentionDetail(f.Job, latestInput.ID, agentRunAttention(*latestInput)))
		}
	}
	if latestTurnStart != nil && latestTurnStart.State != spine.AgentRunCompleted {
		if latestTurnStart.State == spine.AgentRunActive {
			return work(WorkObserveAgent, latestTurnStart.ID, attentionDetail(f.Job, latestTurnStart.ID, ""))
		}
		// A pending or submitting Run without a delivery candidate cannot be
		// executed safely. In particular, submission must be reconciled rather
		// than letting Checks race its harness mutation.
		return work(WorkAttention, latestTurnStart.ID, attentionDetail(f.Job, latestTurnStart.ID, agentRunAttention(*latestTurnStart)))
	}

	// A completed implementation Turn is not a Git fact. Its immutable
	// git-revision Evidence is the recovery boundary, even when HEAD is unchanged.
	if candidate := revisionCandidate(f, latestTurnStart); candidate != nil {
		if candidate.InputRevision != f.Job.Revision {
			return work(WorkAttention, candidate.ID, fmt.Sprintf("AgentRun input Revision %s does not match current Revision %s", candidate.InputRevision, f.Job.Revision))
		}
		return work(WorkObserveRevision, candidate.ID, attentionDetail(f.Job, candidate.ID, ""))
	}
	if id, detail := unchangedAttention(f); id != "" {
		return work(WorkAttention, id, detail)
	}

	// Checks are exact-Revision facts. A failed Check first creates an ordinary
	// workflow Message; delivery and Git observation above then handle its loop.
	if len(f.DeclaredChecks) == 0 {
		return work(WorkAttention, f.Job.Revision, "repository setup declared no deterministic Checks")
	}
	checks := make(map[string]spine.Check)
	for _, check := range f.Checks {
		if check.Revision == f.Job.Revision {
			checks[check.Name] = check
		}
	}
	for _, declaration := range f.DeclaredChecks {
		check, ok := checks[declaration.Name]
		if !ok || check.State == "running" {
			id := spine.CheckID(f.Job.ID, f.Job.Revision, declaration.Name)
			return work(WorkRunChecks, id, attentionDetail(f.Job, id, declaration.Name))
		}
		if check.State == "failed" {
			return work(WorkRunChecks, check.ID, declaration.Name)
		}
		if check.State != "passed" || check.EvidenceID == "" {
			return work(WorkAttention, check.ID, fmt.Sprintf("Check %s has incomplete result %q", check.Name, check.State))
		}
	}

	// The ReviewPlan is the final deterministic decision. There is no pending
	// plan or separate "Checks verified" handoff fact.
	if plan == nil {
		return work(WorkChooseReview, f.Job.Revision, attentionDetail(f.Job, spine.ReviewPolicyAttentionSource(f.Job.Revision), ""))
	}

	if f.Proposal != nil && f.Proposal.ProposedRevision == f.Job.Revision {
		return work(WorkObserveProposal, f.Proposal.URL, fmt.Sprintf("pull request #%d", f.Proposal.Number))
	}
	return work(WorkPublishProposal, f.Job.Revision, "")
}

func reviewFeedbackReturned(deliveries []spine.Delivery, jobID, runID string) bool {
	expectedID := spine.MessageID(jobID, spine.MessageFromAgent, runID)
	for _, delivery := range deliveries {
		message := delivery.Message
		if message.ID == expectedID && message.JobID == jobID && message.FromKind == spine.MessageFromAgent && message.FromID == runID && message.Intent == spine.MessageFollow {
			return true
		}
	}
	return false
}

func actionSucceeded(actions []spine.Action, kind spine.ActionKind, scope string) bool {
	for _, action := range actions {
		if action.Kind == kind && action.Scope == scope {
			return action.State == spine.ActionSucceeded
		}
	}
	return false
}

func publicationPending(actions []spine.Action, revision string) bool {
	for _, action := range actions {
		if action.Scope == revision && action.State != spine.ActionSucceeded &&
			(action.Kind == spine.ActionRepositoryPush || action.Kind == spine.ActionGitHubPullRequest) {
			return true
		}
	}
	return false
}

func publicationDetail(f Snapshot, fallback string) string {
	for _, action := range f.Actions {
		if action.Scope == f.Job.Revision && action.ID == f.Job.WorkflowAttentionSource &&
			(action.Kind == spine.ActionRepositoryPush || action.Kind == spine.ActionGitHubPullRequest) {
			return f.Job.WorkflowAttention
		}
	}
	return fallback
}

func attentionDetail(job spine.CodingJob, source, fallback string) string {
	if job.WorkflowAttentionSource == source && job.WorkflowAttention != "" {
		return job.WorkflowAttention
	}
	return fallback
}

func agentRunAttention(run spine.AgentRun) string {
	if run.Attention != "" {
		return run.Attention
	}
	return fmt.Sprintf("AgentRun %s is %s", run.ID, run.State)
}

func revisionCandidate(f Snapshot, latestTurnStart *spine.AgentRun) *spine.AgentRun {
	observed := make(map[string]bool)
	for _, record := range f.Evidence {
		if record.Kind == "git-revision" {
			observed[record.AgentRunID] = true
		}
	}
	if latestTurnStart == nil || latestTurnStart.State != spine.AgentRunCompleted || observed[latestTurnStart.ID] {
		return nil
	}
	return latestTurnStart
}

func latestImplementationRuns(f Snapshot) (latestInput, latestTurnStart *spine.AgentRun) {
	var latestInputSequence int64
	var latestTurnStartSequence int64
	for i := range f.Deliveries {
		delivery := &f.Deliveries[i]
		run := &delivery.AgentRun
		message := delivery.Message
		if run.Role != "implement" || message.ID != run.MessageID {
			continue
		}
		if latestInput == nil || message.Sequence >= latestInputSequence {
			latestInput, latestInputSequence = run, message.Sequence
		}
		if spine.StartsImplementationTurn(message, *run) && (latestTurnStart == nil || message.Sequence >= latestTurnStartSequence) {
			latestTurnStart, latestTurnStartSequence = run, message.Sequence
		}
	}
	return latestInput, latestTurnStart
}

// unchangedAttention distinguishes an intentionally adjudicated review or
// Proposal feedback batch from an initial request or failed-Check response
// that returned without fixing anything. Equality is already carried by the
// AgentRun input Revision and its git-revision Evidence.
func unchangedAttention(f Snapshot) (string, string) {
	sequences := make(map[string]int64, len(f.Deliveries))
	runs := make(map[string]spine.AgentRun, len(f.Deliveries))
	implementationMessages := make(map[string]bool)
	for _, delivery := range f.Deliveries {
		run := delivery.AgentRun
		sequences[delivery.Message.ID] = delivery.Message.Sequence
		runs[run.ID] = run
		if run.Role == "implement" {
			implementationMessages[run.MessageID] = true
		}
	}
	type observation struct {
		sequence int64
		run      spine.AgentRun
		evidence spine.Evidence
	}
	var observations []observation
	for _, record := range f.Evidence {
		run, ok := runs[record.AgentRunID]
		if !ok || record.Kind != "git-revision" {
			continue
		}
		observations = append(observations, observation{sequences[run.MessageID], run, record})
	}
	if len(observations) == 0 {
		return "", ""
	}
	sort.Slice(observations, func(i, j int) bool { return observations[i].sequence < observations[j].sequence })
	last := observations[len(observations)-1]
	if last.run.InputRevision == "" || last.run.InputRevision != last.evidence.Revision {
		return "", ""
	}
	var previous int64
	if len(observations) > 1 {
		previous = observations[len(observations)-2].sequence
	}
	exactProposal := f.Proposal != nil && f.Proposal.ProposedRevision == f.Job.Revision
	for _, delivery := range f.Deliveries {
		message := delivery.Message
		if message.Sequence <= previous || message.Sequence > last.sequence || !implementationMessages[message.ID] {
			continue
		}
		if message.FromKind == spine.MessageFromAgent || message.FromKind == spine.MessageFromHuman && exactProposal {
			continue
		}
		return last.run.ID, fmt.Sprintf("AgentRun %s handled Message %d without a new committed Revision", last.run.ID, message.Sequence)
	}
	return "", ""
}
