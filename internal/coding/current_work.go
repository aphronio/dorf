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
	WorkWaitAgent       WorkKind = "wait-agent"
	WorkRecordReview    WorkKind = "record-review-feedback"
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
	if w.Kind == WorkWaitAgent {
		return "Awaiting Agent result"
	}
	if w.Kind == WorkRecordReview {
		return "Record reviewer feedback"
	}
	return definition.OperationLabel(string(w.Kind), humanizeIdentifier(string(w.Kind)))
}

// Snapshot is Dorf's concrete, coding-specific read model. It is loaded once
// for inspection or one RunJob decision, then projected without another
// database read. The reads are not a database transaction; every operation
// still revalidates its owning fact before recording an effect.
type Snapshot struct {
	Job         Job
	Sandboxes   []core.Sandbox
	MainSandbox core.Sandbox
	Actions     []core.Action
	Messages    []MessageRecord
	ReviewRuns  []ReviewRunView
	Revisions   []Revision
	Evidence    []core.Evidence
	ReviewPlans []ReviewPlanRecord
	Proposal    *Proposal
	Outcome     *Outcome
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

func decideCurrentWork(f Snapshot) Work {
	return decideCurrentWorkWithReviewRuns(f, f.currentReviewRuns())
}

func decideCurrentWorkWithReviewRuns(f Snapshot, reviewRuns []ReviewRunView) Work {
	if f.Outcome != nil {
		return f.work(WorkComplete, f.Outcome.JobID, string(f.Outcome.Kind))
	}
	if !f.Job.AdmissionOpen {
		return f.work(WorkComplete, f.Job.ID, "admission closed")
	}
	if work, ok := mainSandboxWork(f); ok {
		return work
	}
	if f.Proposal == nil && publicationPending(f.Actions, f.Job.Revision) {
		return f.work(WorkPublishProposal, f.Job.Revision, publicationDetail(f, "reconcile started publication"))
	}

	plan := f.currentReviewPlan()
	if plan != nil {
		if work, ok := selectedReviewWork(f, plan, reviewRuns); ok {
			return work
		}
	}
	if work, ok := implementationWork(f); ok {
		return work
	}

	if plan == nil {
		return f.work(WorkChooseReview, f.Job.Revision, attentionDetail(f.Job, ReviewPolicyAttentionSource(f.Job.Revision), ""))
	}

	if f.Proposal != nil && f.Proposal.ProposedRevision == f.Job.Revision {
		return f.work(WorkObserveProposal, f.Proposal.URL, fmt.Sprintf("pull request #%d", f.Proposal.Number))
	}
	return f.work(WorkPublishProposal, f.Job.Revision, "")
}

func (f Snapshot) work(kind WorkKind, factID, detail string) Work {
	return Work{Kind: kind, Revision: f.Job.Revision, FactID: factID, Detail: detail}
}

func (f Snapshot) actionWork(kind core.ActionKind, scope, detail string) Work {
	work := Work{Kind: WorkAction, Revision: f.Job.Revision, FactID: core.ScopedActionID(f.Job.ID, kind, scope), ActionKind: kind, Scope: scope, Detail: detail}
	if f.Job.WorkflowAttentionSource == work.FactID && f.Job.WorkflowAttention != "" {
		work.Kind = WorkAttention
		work.Detail = f.Job.WorkflowAttention
	}
	return work
}

func mainSandboxWork(f Snapshot) (Work, bool) {
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, gitworkspace.ActionRepositoryClone, core.ActionRouteCreate} {
		if !core.HasSucceededAction(f.Actions, kind, f.MainSandbox.ID) {
			return f.actionWork(kind, f.MainSandbox.ID, ""), true
		}
	}
	return Work{}, false
}

func selectedReviewWork(f Snapshot, plan *ReviewPlanRecord, reviewRuns []ReviewRunView) (Work, bool) {
	byRole := make(map[string]ReviewRunView, len(reviewRuns))
	for _, run := range reviewRuns {
		byRole[run.Role] = run
	}
	for _, role := range plan.Plan.Roles {
		run, ok := byRole[string(role)]
		if !ok {
			return f.work(WorkAttention, f.Job.Revision, fmt.Sprintf("selected reviewer %s has no durable execution", role)), true
		}
		if reviewFeedbackReturned(f.Messages, f.Job.ID, run.ID) {
			continue
		}
		if f.Job.WorkflowAttentionSource == run.MessageID && f.Job.WorkflowAttention != "" {
			return f.work(WorkAttention, run.MessageID, f.Job.WorkflowAttention), true
		}
		if run.Attention != "" || run.Outcome != "" && run.Outcome != "completed" {
			return f.work(WorkAttention, run.MessageID, attentionDetail(f.Job, run.MessageID, messageAttention(run.MessageID, run.Outcome, run.Attention))), true
		}
		if !exactReviewSandbox(f.Job.ID, run) {
			return f.work(WorkAttention, run.ID, fmt.Sprintf("selected reviewer %s has no exact Job-owned Sandbox", role)), true
		}
		for _, kind := range []core.ActionKind{core.ActionSandboxCreate, ActionReviewCheckout, core.ActionRouteCreate} {
			if !core.HasSucceededAction(f.Actions, kind, run.Sandbox.ID) {
				return f.actionWork(kind, run.Sandbox.ID, string(role)), true
			}
		}
		if run.Outcome == "completed" {
			return f.work(WorkRecordReview, run.MessageID, string(role)), true
		}
		return f.work(WorkWaitAgent, run.MessageID, "awaiting "+string(role)+" reviewer result"), true
	}
	return Work{}, false
}

func exactReviewSandbox(jobID string, run ReviewRunView) bool {
	return run.Sandbox.ID != "" && run.Sandbox.ID == run.SandboxID && run.Sandbox.JobID == jobID
}

func implementationWork(f Snapshot) (Work, bool) {
	latestInput, latestTurnStart := latestImplementationRuns(f)
	for _, message := range []*MessageRecord{latestInput, latestTurnStart} {
		if message == nil {
			continue
		}
		if message.Outcome == "" && message.Attention == "" {
			return f.work(WorkWaitAgent, message.Message.ID, "awaiting implementation result"), true
		}
		if message.Outcome != "completed" {
			return f.work(WorkAttention, message.Message.ID, attentionDetail(f.Job, message.Message.ID, messageAttention(message.Message.ID, message.Outcome, message.Attention))), true
		}
	}
	if candidate := revisionCandidate(f, latestTurnStart); candidate != nil {
		if candidate.InputRevision != f.Job.Revision {
			return f.work(WorkAttention, candidate.Message.ID, fmt.Sprintf("Message input Revision %s does not match current Revision %s", candidate.InputRevision, f.Job.Revision)), true
		}
		return f.work(WorkObserveRevision, candidate.Message.ID, attentionDetail(f.Job, candidate.Message.ID, "")), true
	}
	if id, detail := unchangedAttention(f); id != "" {
		return f.work(WorkAttention, id, detail), true
	}
	return Work{}, false
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
// accepted input Revision and its git-revision Evidence.
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
		// A Steer settles inside its exact target Turn and shares that Turn's Git
		// observation boundary; it never creates a separate Revision boundary.
		if message.FromKind == core.MessageFromAgent || message.Intent == core.MessageSteer || message.FromKind == core.MessageFromHuman && exactProposal {
			continue
		}
		return last.message.Message.ID, fmt.Sprintf("Message %s was handled without a new committed Revision", last.message.Message.ID)
	}
	return "", ""
}
