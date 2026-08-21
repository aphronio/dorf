package coding

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

// WorkKind is the next concrete operation in Dorf's coding workflow. It is a
// disposable answer derived from product facts, never a value stored on Job.
type WorkKind string

const (
	WorkComplete        WorkKind = "complete"
	WorkAttention       WorkKind = "attention"
	WorkAction          WorkKind = "action"
	WorkRunReviewer     WorkKind = "run-reviewer"
	WorkAgentMessage    WorkKind = "reconcile-agent-message"
	WorkObserveRevision WorkKind = "observe-revision"
	WorkChooseReview    WorkKind = "choose-review"
	WorkPublishProposal WorkKind = "publish-proposal"
	WorkObserveProposal WorkKind = "observe-proposal"
)

// Work names the natural fact which owns the next operation. FactID and Scope
// identify that fact and its concrete resource when required. Work is useful
// for explanation and exact execution, but deliberately not durable.
type Work struct {
	Kind       WorkKind        `json:"kind"`
	Revision   string          `json:"revision,omitempty"`
	FactID     string          `json:"fact_id,omitempty"`
	ActionKind core.ActionKind `json:"action,omitempty"`
	Scope      string          `json:"scope,omitempty"`
	Detail     string          `json:"detail,omitempty"`
}

func (w Work) Description() string {
	definition := WorkflowDefinition()
	if w.Kind == WorkAction {
		return definition.ActionLabel(w.ActionKind)
	}
	if w.Kind == WorkAgentMessage {
		return definition.AgentRoleLabel("implement") + " working"
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
	Job          Job
	Sandboxes    []core.Sandbox
	MainSandbox  core.Sandbox
	Actions      []core.Action
	AgentMessage *core.AgentMessageWork
	Messages     []MessageRecord
	ReviewRuns   []ReviewRunView
	Revisions    []Revision
	Evidence     []core.Evidence
	ReviewPlans  []ReviewPlanRecord
	Proposal     *Proposal
	Outcome      *Outcome
}

// Projection is a disposable explanation derived from one Snapshot. It is
// shared by execution and inspection and is never persisted.
type Projection struct {
	CurrentWork Work                `json:"current_work"`
	Readiness   ReadinessAssessment `json:"readiness"`
}

// Project derives readiness and the one next coding operation once. Once
// publication starts, its admitted input boundary is retained so a later
// accepted Message cannot strand reconciliation.
func (s Snapshot) Project(evidenceStore blob.Store) (Projection, error) {
	messages := PublicationMessages(s.Messages, publicationIntentAt(s.Actions, s.Job.Revision))
	reviewRuns := s.currentReviewRuns()
	readiness := AssessReviewReadiness(s.Job, s.Evidence, evidenceStore, s.currentReviewPlan(), reviewRuns, messages)
	work := decideCurrentWorkWithReviewRuns(s, reviewRuns)
	if work.Kind == WorkPublishProposal && !readiness.Ready {
		work.Kind = WorkAttention
		work.Detail = "publication lost exact-Revision readiness: " + readiness.Reason
	}
	return Projection{CurrentWork: work, Readiness: readiness}, nil
}

func publicationIntentAt(actions []core.Action, revision string) time.Time {
	for _, action := range actions {
		if action.Kind == ActionGitHubPullRequest && action.Scope == revision {
			return action.CreatedAt
		}
	}
	return time.Time{}
}

// LoadSnapshot performs one staged load of the coding product facts.
func LoadSnapshot(ctx context.Context, store Store, jobID string) (Snapshot, error) {
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
		return snapshot, fmt.Errorf("main Sandbox %s is not durably reserved", core.MainSandboxName(jobID))
	}
	snapshot.Actions, err = store.Actions(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	snapshot.Messages, snapshot.ReviewRuns, err = store.CodingMessages(ctx, jobID)
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
		snapshot.AgentMessage, err = store.CodingAgentMessage(ctx, jobID)
		if err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func (s Snapshot) currentReviewPlan() *ReviewPlanRecord {
	for i := range s.ReviewPlans {
		if s.ReviewPlans[i].Revision == s.Job.Revision {
			return &s.ReviewPlans[i]
		}
	}
	return nil
}

func (s Snapshot) currentReviewRuns() []ReviewRunView {
	runs := make([]ReviewRunView, 0, len(s.ReviewRuns))
	for _, run := range s.ReviewRuns {
		if run.InputRevision == s.Job.Revision {
			runs = append(runs, run)
		}
	}
	return runs
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
	return decideCurrentWorkWithReviewRuns(f, f.currentReviewRuns())
}

func decideCurrentWorkWithReviewRuns(f Snapshot, reviewRuns []ReviewRunView) Work {
	work := func(kind WorkKind, factID, detail string) Work {
		return Work{Kind: kind, Revision: f.Job.Revision, FactID: factID, Detail: detail}
	}
	actionWork := func(kind core.ActionKind, scope, detail string) Work {
		work := Work{
			Kind: WorkAction, Revision: f.Job.Revision,
			FactID:     core.ScopedActionID(f.Job.ID, kind, scope),
			ActionKind: kind, Scope: scope, Detail: detail,
		}
		if f.Job.WorkflowAttentionSource == work.FactID && f.Job.WorkflowAttention != "" {
			work.Kind = WorkAttention
			work.Detail = f.Job.WorkflowAttention
		}
		return work
	}
	if f.Outcome != nil {
		return work(WorkComplete, f.Outcome.JobID, string(f.Outcome.Kind))
	}
	if !f.Job.AdmissionOpen {
		return work(WorkComplete, f.Job.ID, "admission closed")
	}

	// Infrastructure is a fixed prerequisite chain. Sandbox-create truthfully
	// projects Core custody; the workflow never executes its provider mutation.
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
		byRole := make(map[string]ReviewRunView, len(reviewRuns))
		for _, run := range reviewRuns {
			byRole[run.Role] = run
		}
		for _, role := range plan.Plan.Roles {
			run, ok := byRole[string(role)]
			if !ok {
				return work(WorkAttention, f.Job.Revision, fmt.Sprintf("selected reviewer %s has no AgentRun", role))
			}
			if reviewFeedbackReturned(f.Messages, f.Job.ID, run.ID) {
				continue
			}
			if f.Job.WorkflowAttentionSource == run.MessageID && f.Job.WorkflowAttention != "" {
				return work(WorkAttention, run.MessageID, f.Job.WorkflowAttention)
			}
			if run.Attention != "" || run.Outcome != "" && run.Outcome != "completed" {
				return work(WorkAttention, run.MessageID, attentionDetail(f.Job, run.MessageID, messageAttention(run.MessageID, run.Outcome, run.Attention)))
			}
			if run.Sandbox.ID == "" || run.Sandbox.ID != run.SandboxID || run.Sandbox.JobID != f.Job.ID {
				return work(WorkAttention, run.ID, fmt.Sprintf("selected reviewer %s has no exact Job-owned Sandbox", role))
			}
			if !actionSucceeded(f.Actions, core.ActionSandboxCreate, run.Sandbox.ID) {
				return actionWork(core.ActionSandboxCreate, run.Sandbox.ID, string(role))
			}
			if !actionSucceeded(f.Actions, ActionReviewCheckout, run.Sandbox.ID) {
				return actionWork(ActionReviewCheckout, run.Sandbox.ID, string(role))
			}
			if !actionSucceeded(f.Actions, core.ActionRouteCreate, run.Sandbox.ID) {
				return actionWork(core.ActionRouteCreate, run.Sandbox.ID, string(role))
			}
			return Work{Kind: WorkRunReviewer, Revision: f.Job.Revision, FactID: run.MessageID, Scope: run.Sandbox.ID, Detail: string(role)}
		}
	}

	// Workflows select only the exact Message and Sandbox. Core owns whether
	// this cycle submits, steers, recovers, or observes the internal AgentRun.
	if f.AgentMessage != nil {
		return Work{Kind: WorkAgentMessage, Revision: f.Job.Revision, FactID: f.AgentMessage.MessageID, Scope: f.AgentMessage.SandboxID}
	}
	latestInput, latestTurnStart := latestImplementationRuns(f)
	if latestInput != nil && latestInput.Outcome != "completed" {
		return work(WorkAttention, latestInput.Message.ID, attentionDetail(f.Job, latestInput.Message.ID, messageAttention(latestInput.Message.ID, latestInput.Outcome, latestInput.Attention)))
	}
	if latestTurnStart != nil && latestTurnStart.Outcome != "completed" {
		// A pending or submitting Run without a delivery candidate cannot be
		// executed safely. In particular, submission must be reconciled rather
		// than letting later workflow decisions race its harness mutation.
		return work(WorkAttention, latestTurnStart.Message.ID, attentionDetail(f.Job, latestTurnStart.Message.ID, messageAttention(latestTurnStart.Message.ID, latestTurnStart.Outcome, latestTurnStart.Attention)))
	}

	// A completed implementation Turn is not a Git fact. Its immutable
	// git-revision Evidence is the recovery boundary, even when HEAD is unchanged.
	if candidate := revisionCandidate(f, latestTurnStart); candidate != nil {
		if candidate.InputRevision != f.Job.Revision {
			return work(WorkAttention, candidate.Message.ID, fmt.Sprintf("Message input Revision %s does not match current Revision %s", candidate.InputRevision, f.Job.Revision))
		}
		return work(WorkObserveRevision, candidate.Message.ID, attentionDetail(f.Job, candidate.Message.ID, ""))
	}
	if id, detail := unchangedAttention(f); id != "" {
		return work(WorkAttention, id, detail)
	}

	// Deterministic verification is intentionally absent here. D084 records
	// when workflow-owned verification is earned and where to recover the
	// useful pre-deletion mechanics without restoring the old repository contract.

	// The ReviewPlan is the final deterministic decision. There is no pending
	// plan or a separate handoff fact.
	if plan == nil {
		return work(WorkChooseReview, f.Job.Revision, attentionDetail(f.Job, ReviewPolicyAttentionSource(f.Job.Revision), ""))
	}

	if f.Proposal != nil && f.Proposal.ProposedRevision == f.Job.Revision {
		return work(WorkObserveProposal, f.Proposal.URL, fmt.Sprintf("pull request #%d", f.Proposal.Number))
	}
	return work(WorkPublishProposal, f.Job.Revision, "")
}

func reviewFeedbackReturned(messages []MessageRecord, jobID, runID string) bool {
	expectedID := core.MessageID(jobID, core.MessageFromAgent, runID)
	for _, record := range messages {
		message := record.Message
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
			(action.Kind == ActionRepositoryPush || action.Kind == ActionGitHubPullRequest) {
			return true
		}
	}
	return false
}

func publicationDetail(f Snapshot, fallback string) string {
	for _, action := range f.Actions {
		if action.Scope == f.Job.Revision && action.ID == f.Job.WorkflowAttentionSource &&
			(action.Kind == ActionRepositoryPush || action.Kind == ActionGitHubPullRequest) {
			return f.Job.WorkflowAttention
		}
	}
	return fallback
}

func attentionDetail(job Job, source, fallback string) string {
	if job.WorkflowAttentionSource == source && job.WorkflowAttention != "" {
		return job.WorkflowAttention
	}
	return fallback
}

func messageAttention(messageID, outcome, attention string) string {
	if attention != "" {
		return attention
	}
	if outcome != "" {
		return fmt.Sprintf("Message %s ended with outcome %s", messageID, outcome)
	}
	return fmt.Sprintf("Message %s still needs reconciliation", messageID)
}

func revisionCandidate(f Snapshot, latestTurnStart *MessageRecord) *MessageRecord {
	observed := make(map[string]bool)
	for _, record := range f.Evidence {
		if record.Kind == "git-revision" {
			observed[record.AgentRunID] = true
		}
	}
	if latestTurnStart == nil || latestTurnStart.Outcome != "completed" || observed[latestTurnStart.ProducerID] {
		return nil
	}
	return latestTurnStart
}

func latestImplementationRuns(f Snapshot) (latestInput, latestTurnStart *MessageRecord) {
	var latestInputSequence int64
	var latestTurnStartSequence int64
	for i := range f.Messages {
		record := &f.Messages[i]
		message := record.Message
		if latestInput == nil || message.Sequence >= latestInputSequence {
			latestInput, latestInputSequence = record, message.Sequence
		}
		if record.StartsTurn && (latestTurnStart == nil || message.Sequence >= latestTurnStartSequence) {
			latestTurnStart, latestTurnStartSequence = record, message.Sequence
		}
	}
	return latestInput, latestTurnStart
}

// unchangedAttention distinguishes an intentionally adjudicated review or
// Proposal feedback batch from an initial request or reviewer response
// that returned without fixing anything. Equality is already carried by the
// AgentRun input Revision and its git-revision Evidence.
func unchangedAttention(f Snapshot) (string, string) {
	sequences := make(map[string]int64, len(f.Messages))
	producers := make(map[string]MessageRecord, len(f.Messages))
	for _, message := range f.Messages {
		sequences[message.Message.ID] = message.Message.Sequence
		producers[message.ProducerID] = message
	}
	type observation struct {
		sequence int64
		message  MessageRecord
		evidence core.Evidence
	}
	var observations []observation
	for _, record := range f.Evidence {
		message, ok := producers[record.AgentRunID]
		if !ok || record.Kind != "git-revision" {
			continue
		}
		observations = append(observations, observation{sequences[message.Message.ID], message, record})
	}
	if len(observations) == 0 {
		return "", ""
	}
	sort.Slice(observations, func(i, j int) bool { return observations[i].sequence < observations[j].sequence })
	last := observations[len(observations)-1]
	if last.message.InputRevision == "" || last.message.InputRevision != last.evidence.Revision {
		return "", ""
	}
	var previous int64
	if len(observations) > 1 {
		previous = observations[len(observations)-2].sequence
	}
	exactProposal := f.Proposal != nil && f.Proposal.ProposedRevision == f.Job.Revision
	for _, record := range f.Messages {
		message := record.Message
		if message.Sequence <= previous || message.Sequence > last.sequence {
			continue
		}
		if message.FromKind == core.MessageFromAgent || message.FromKind == core.MessageFromHuman && exactProposal {
			continue
		}
		return last.message.Message.ID, fmt.Sprintf("Message %s was handled without a new committed Revision", last.message.Message.ID)
	}
	return "", ""
}
