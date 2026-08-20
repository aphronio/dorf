package workflow

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/postgres"
)

// WorkKind is the next concrete operation in Dorf's coding workflow. It is a
// disposable answer derived from product facts, never a value stored on Job.
type WorkKind string

const (
	WorkComplete        WorkKind = "complete"
	WorkAttention       WorkKind = "attention"
	WorkAction          WorkKind = "action"
	WorkRunReviewer     WorkKind = "run-reviewer"
	WorkDeliverMessage  WorkKind = "deliver-message"
	WorkObserveAgent    WorkKind = "observe-agent-run"
	WorkObserveRevision WorkKind = "observe-revision"
	WorkChooseReview    WorkKind = "choose-review"
	WorkPublishProposal WorkKind = "publish-proposal"
	WorkObserveProposal WorkKind = "observe-proposal"
)

// Work names the natural fact which owns the next operation. FactID is an
// Action, AgentRun, Revision, Proposal, or Outcome identity as
// appropriate. Scope names the concrete resource only when an Action needs
// one. Work is useful for explanation and exact execution, but deliberately
// not durable.
type Work struct {
	Kind       WorkKind        `json:"kind"`
	Revision   string          `json:"revision,omitempty"`
	FactID     string          `json:"fact_id,omitempty"`
	ActionKind core.ActionKind `json:"action,omitempty"`
	Scope      string          `json:"scope,omitempty"`
	Detail     string          `json:"detail,omitempty"`
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
	Job         coding.Job
	Sandboxes   []core.Sandbox
	MainSandbox core.Sandbox
	Actions     []core.Action
	Delivery    *core.Delivery
	Deliveries  []core.Delivery
	Revisions   []coding.Revision
	Evidence    []core.Evidence
	ReviewPlans []coding.ReviewPlanRecord
	Proposal    *coding.Proposal
	Outcome     *coding.Outcome
}

// Projection is a disposable explanation derived from one Snapshot. It is
// shared by execution and inspection and is never persisted.
type Projection struct {
	CurrentWork Work                       `json:"current_work"`
	Readiness   coding.ReadinessAssessment `json:"readiness"`
}

// Project derives readiness and the one next coding operation once. Once
// publication starts, its admitted input boundary is retained so a later
// accepted Message cannot strand reconciliation.
func (s Snapshot) Project(evidenceStore blob.Store) (Projection, error) {
	deliveries := coding.PublicationDeliveries(s.Deliveries, publicationIntentAt(s.Actions, s.Job.Revision))
	reviewRuns, err := s.currentReviewRuns()
	if err != nil {
		return Projection{}, err
	}
	readiness := coding.AssessReviewReadiness(s.Job, s.Evidence, evidenceStore, s.currentReviewPlan(), reviewRuns, deliveries)
	work := decideCurrentWorkWithReviewRuns(s, reviewRuns)
	if work.Kind == WorkPublishProposal && !readiness.Ready {
		work.Kind = WorkAttention
		work.Detail = "publication lost exact-Revision readiness: " + readiness.Reason
	}
	return Projection{CurrentWork: work, Readiness: readiness}, nil
}

func publicationIntentAt(actions []core.Action, revision string) time.Time {
	for _, action := range actions {
		if action.Kind == coding.ActionGitHubPullRequest && action.Scope == revision {
			return action.CreatedAt
		}
	}
	return time.Time{}
}

// LoadSnapshot performs one staged load of the coding product facts.
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
		if sandbox.ID == core.MainSandboxName(jobID) {
			snapshot.MainSandbox = sandbox
			break
		}
	}
	if snapshot.MainSandbox.ID == "" {
		return snapshot, fmt.Errorf("main Sandbox %s: %w", core.MainSandboxName(jobID), postgres.ErrNotFound)
	}
	snapshot.Actions, err = store.Actions(ctx, jobID)
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
	if snapshot.Outcome == nil && snapshot.Job.AdmissionOpen {
		snapshot.Delivery, err = store.DeliveryCandidate(ctx, jobID)
		if err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func (s Snapshot) currentReviewPlan() *coding.ReviewPlanRecord {
	for i := range s.ReviewPlans {
		if s.ReviewPlans[i].Revision == s.Job.Revision {
			return &s.ReviewPlans[i]
		}
	}
	return nil
}

func (s Snapshot) currentReviewRuns() ([]coding.ReviewRunView, error) {
	all, err := coding.ReviewRuns(s.Deliveries, s.Sandboxes)
	if err != nil {
		return nil, err
	}
	runs := make([]coding.ReviewRunView, 0, len(all))
	for _, run := range all {
		if run.InputRevision == s.Job.Revision {
			runs = append(runs, run)
		}
	}
	return runs, nil
}

func codingPrerequisitesComplete(f Snapshot) bool {
	return actionSucceeded(f.Actions, core.ActionSandboxCreate, f.MainSandbox.ID) &&
		actionSucceeded(f.Actions, gitworkspace.ActionRepositoryClone, f.MainSandbox.ID) &&
		actionSucceeded(f.Actions, core.ActionRouteCreate, f.MainSandbox.ID)
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

func decideCurrentWorkWithReviewRuns(f Snapshot, reviewRuns []coding.ReviewRunView) Work {
	work := func(kind WorkKind, factID, detail string) Work {
		return Work{Kind: kind, Revision: f.Job.Revision, FactID: factID, Detail: detail}
	}
	actionWork := func(kind core.ActionKind, scope, detail string) Work {
		return Work{
			Kind: WorkAction, Revision: f.Job.Revision,
			FactID:     core.ScopedActionID(f.Job.ID, kind, scope),
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
	if !actionSucceeded(f.Actions, core.ActionSandboxCreate, f.MainSandbox.ID) {
		return actionWork(core.ActionSandboxCreate, f.MainSandbox.ID, "")
	}
	if !actionSucceeded(f.Actions, gitworkspace.ActionRepositoryClone, f.MainSandbox.ID) {
		return actionWork(gitworkspace.ActionRepositoryClone, f.MainSandbox.ID, "")
	}
	if !actionSucceeded(f.Actions, core.ActionRouteCreate, f.MainSandbox.ID) {
		return actionWork(core.ActionRouteCreate, f.MainSandbox.ID, "")
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
		byRole := make(map[string]coding.ReviewRunView, len(reviewRuns))
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
			if run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted || run.State == core.AgentRunUncertain {
				return work(WorkAttention, run.ID, attentionDetail(f.Job, run.ID, agentRunAttention(run.AgentRun)))
			}
			if run.Sandbox.ID == "" || run.Sandbox.ID != run.SandboxID || run.Sandbox.JobID != f.Job.ID {
				return work(WorkAttention, run.ID, fmt.Sprintf("selected reviewer %s has no exact Job-owned Sandbox", role))
			}
			if !actionSucceeded(f.Actions, core.ActionSandboxCreate, run.Sandbox.ID) {
				return actionWork(core.ActionSandboxCreate, run.Sandbox.ID, string(role))
			}
			if !actionSucceeded(f.Actions, coding.ActionReviewCheckout, run.Sandbox.ID) {
				return actionWork(coding.ActionReviewCheckout, run.Sandbox.ID, string(role))
			}
			if !actionSucceeded(f.Actions, core.ActionRouteCreate, run.Sandbox.ID) {
				return actionWork(core.ActionRouteCreate, run.Sandbox.ID, string(role))
			}
			return work(WorkRunReviewer, run.ID, string(role))
		}
	}

	// Messages share one implementation lane: steers may target the active
	// Turn; turn-starting follows remain FIFO. DeliveryCandidate is read-only.
	if f.Delivery != nil {
		run := f.Delivery.AgentRun
		if run.State == core.AgentRunFailed || run.State == core.AgentRunInterrupted || run.State == core.AgentRunUncertain {
			return work(WorkAttention, run.ID, attentionDetail(f.Job, run.ID, agentRunAttention(run)))
		}
		return work(WorkDeliverMessage, run.ID, fmt.Sprintf("Message %d", f.Delivery.Message.Sequence))
	}
	latestInput, latestTurnStart := latestImplementationRuns(f)
	if latestInput != nil && latestInput.State != core.AgentRunCompleted {
		if latestTurnStart != nil && latestInput.ID == latestTurnStart.ID && latestInput.State == core.AgentRunActive {
			// The active turn-starting input is observed below. A later unsettled
			// steer remains attention unless it is selected for delivery above.
		} else {
			// A failed steer may have no Turn identity when its terminal-target
			// fallback failed before binding. It is still the latest accepted input
			// and must not be skipped in favor of an older observed Turn.
			return work(WorkAttention, latestInput.ID, attentionDetail(f.Job, latestInput.ID, agentRunAttention(*latestInput)))
		}
	}
	if latestTurnStart != nil && latestTurnStart.State != core.AgentRunCompleted {
		if latestTurnStart.State == core.AgentRunActive {
			return work(WorkObserveAgent, latestTurnStart.ID, attentionDetail(f.Job, latestTurnStart.ID, ""))
		}
		// A pending or submitting Run without a delivery candidate cannot be
		// executed safely. In particular, submission must be reconciled rather
		// than letting later workflow decisions race its harness mutation.
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

	// The ReviewPlan is the final deterministic decision. There is no pending
	// plan or a separate handoff fact.
	if plan == nil {
		return work(WorkChooseReview, f.Job.Revision, attentionDetail(f.Job, coding.ReviewPolicyAttentionSource(f.Job.Revision), ""))
	}

	if f.Proposal != nil && f.Proposal.ProposedRevision == f.Job.Revision {
		return work(WorkObserveProposal, f.Proposal.URL, fmt.Sprintf("pull request #%d", f.Proposal.Number))
	}
	return work(WorkPublishProposal, f.Job.Revision, "")
}

func reviewFeedbackReturned(deliveries []core.Delivery, jobID, runID string) bool {
	expectedID := core.MessageID(jobID, core.MessageFromAgent, runID)
	for _, delivery := range deliveries {
		message := delivery.Message
		if message.ID == expectedID && message.JobID == jobID && message.FromKind == core.MessageFromAgent && message.FromID == runID && message.Intent == core.MessageFollow {
			return true
		}
	}
	return false
}

func actionSucceeded(actions []core.Action, kind core.ActionKind, scope string) bool {
	for _, action := range actions {
		if action.Kind == kind && action.Scope == scope {
			return action.State == core.ActionSucceeded
		}
	}
	return false
}

func publicationPending(actions []core.Action, revision string) bool {
	for _, action := range actions {
		if action.Scope == revision && action.State != core.ActionSucceeded &&
			(action.Kind == coding.ActionRepositoryPush || action.Kind == coding.ActionGitHubPullRequest) {
			return true
		}
	}
	return false
}

func publicationDetail(f Snapshot, fallback string) string {
	for _, action := range f.Actions {
		if action.Scope == f.Job.Revision && action.ID == f.Job.WorkflowAttentionSource &&
			(action.Kind == coding.ActionRepositoryPush || action.Kind == coding.ActionGitHubPullRequest) {
			return f.Job.WorkflowAttention
		}
	}
	return fallback
}

func attentionDetail(job coding.Job, source, fallback string) string {
	if job.WorkflowAttentionSource == source && job.WorkflowAttention != "" {
		return job.WorkflowAttention
	}
	return fallback
}

func agentRunAttention(run core.AgentRun) string {
	if run.Attention != "" {
		return run.Attention
	}
	return fmt.Sprintf("AgentRun %s is %s", run.ID, run.State)
}

func revisionCandidate(f Snapshot, latestTurnStart *core.AgentRun) *core.AgentRun {
	observed := make(map[string]bool)
	for _, record := range f.Evidence {
		if record.Kind == "git-revision" {
			observed[record.AgentRunID] = true
		}
	}
	if latestTurnStart == nil || latestTurnStart.State != core.AgentRunCompleted || observed[latestTurnStart.ID] {
		return nil
	}
	return latestTurnStart
}

func latestImplementationRuns(f Snapshot) (latestInput, latestTurnStart *core.AgentRun) {
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
		if coding.StartsImplementationTurn(message, *run) && (latestTurnStart == nil || message.Sequence >= latestTurnStartSequence) {
			latestTurnStart, latestTurnStartSequence = run, message.Sequence
		}
	}
	return latestInput, latestTurnStart
}

// unchangedAttention distinguishes an intentionally adjudicated review or
// Proposal feedback batch from an initial request or reviewer response
// that returned without fixing anything. Equality is already carried by the
// AgentRun input Revision and its git-revision Evidence.
func unchangedAttention(f Snapshot) (string, string) {
	sequences := make(map[string]int64, len(f.Deliveries))
	runs := make(map[string]core.AgentRun, len(f.Deliveries))
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
		run      core.AgentRun
		evidence core.Evidence
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
		if message.FromKind == core.MessageFromAgent || message.FromKind == core.MessageFromHuman && exactProposal {
			continue
		}
		return last.run.ID, fmt.Sprintf("AgentRun %s handled Message %d without a new committed Revision", last.run.ID, message.Sequence)
	}
	return "", ""
}
