package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

// WorkKind is the next concrete operation in Dorf's coding workflow. It is a
// disposable answer derived from product facts, never a value stored on Job.
type WorkKind string

const (
	WorkComplete        WorkKind = "complete"
	WorkAttention       WorkKind = "attention"
	WorkCreateSandbox   WorkKind = "create-sandbox"
	WorkCloneRepository WorkKind = "clone-repository"
	WorkSetupRepository WorkKind = "setup-repository"
	WorkCreateRoute     WorkKind = "create-route"
	WorkRunReviewer     WorkKind = "run-reviewer"
	WorkDeliverMessage  WorkKind = "deliver-message"
	WorkObserveRevision WorkKind = "observe-revision"
	WorkRunChecks       WorkKind = "run-checks"
	WorkChooseReview    WorkKind = "choose-review"
	WorkPublishProposal WorkKind = "publish-proposal"
	WorkObserveProposal WorkKind = "observe-proposal"
)

// Work names the natural fact which owns the next operation. FactID is an
// Action, AgentRun, Check, Revision, Proposal, or Outcome identity as
// appropriate. It is useful for explanation and exact execution, but Work is
// deliberately not durable.
type Work struct {
	Kind     WorkKind `json:"kind"`
	Revision string   `json:"revision,omitempty"`
	FactID   string   `json:"fact_id,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

func (w Work) Description() string {
	switch w.Kind {
	case WorkComplete:
		return "Complete"
	case WorkAttention:
		return "Needs attention"
	case WorkCreateSandbox:
		return "Provision Sandbox"
	case WorkCloneRepository:
		return "Clone repository"
	case WorkSetupRepository:
		return "Run repository setup"
	case WorkCreateRoute:
		return "Create provider Route"
	case WorkRunReviewer:
		return "Run selected reviewer"
	case WorkDeliverMessage:
		return "Deliver Message to implementation agent"
	case WorkObserveRevision:
		return "Inspect implementation checkout"
	case WorkRunChecks:
		return "Run deterministic Checks"
	case WorkChooseReview:
		return "Choose deterministic review"
	case WorkPublishProposal:
		return "Publish exact-Revision Proposal"
	case WorkObserveProposal:
		return "Observe Proposal feedback or outcome"
	default:
		return string(w.Kind)
	}
}

type currentWorkFacts struct {
	job        spine.Job
	sandbox    spine.Sandbox
	actions    []spine.Action
	setup      *spine.Action
	delivery   *spine.Delivery
	messages   []spine.MessageView
	runs       []spine.AgentRun
	declared   []spine.DeclaredCheck
	checks     []spine.Check
	evidence   []spine.Evidence
	reviewPlan *spine.ReviewPlanRecord
	reviewRuns []spine.ReviewRunView
	proposal   *spine.GitHubProposal
	outcome    *spine.JobOutcome
}

// CurrentWork derives the one coding-workflow answer used by execution and
// inspection. Reads may span snapshots; every operation still revalidates its
// exact owning fact transactionally before recording an effect.
func CurrentWork(ctx context.Context, store postgres.Store, jobID string) (Work, error) {
	facts, err := loadCurrentWorkFacts(ctx, store, jobID)
	if err != nil {
		return Work{}, err
	}
	return decideCurrentWork(facts), nil
}

func loadCurrentWorkFacts(ctx context.Context, store postgres.Store, jobID string) (currentWorkFacts, error) {
	var facts currentWorkFacts
	var err error
	facts.job, err = store.Job(ctx, jobID)
	if err != nil {
		return facts, err
	}
	facts.outcome, err = store.Outcome(ctx, jobID)
	if err != nil {
		return facts, err
	}
	if facts.outcome != nil || !facts.job.AdmissionOpen {
		return facts, nil
	}
	facts.sandbox, err = store.Sandbox(ctx, spine.MainSandboxName(jobID))
	if err != nil {
		return facts, err
	}
	facts.actions, err = store.Actions(ctx, jobID)
	if err != nil {
		return facts, err
	}
	facts.setup, err = store.SelectedSetup(ctx, jobID)
	if err != nil {
		return facts, err
	}
	if !codingPrerequisitesComplete(facts) {
		return facts, nil
	}
	facts.delivery, err = store.DeliveryCandidate(ctx, jobID)
	if err != nil {
		return facts, err
	}
	facts.messages, err = store.Messages(ctx, jobID)
	if err != nil {
		return facts, err
	}
	facts.runs, err = store.AgentRuns(ctx, jobID)
	if err != nil {
		return facts, err
	}
	facts.evidence, err = store.Evidence(ctx, jobID)
	if err != nil {
		return facts, err
	}
	facts.declared, err = store.DeclaredChecks(ctx, jobID)
	if err != nil {
		return facts, err
	}
	facts.checks, err = store.Checks(ctx, jobID)
	if err != nil {
		return facts, err
	}
	plan, planErr := store.ReviewPlan(ctx, jobID, facts.job.Revision)
	if planErr == nil {
		facts.reviewPlan = &plan
		facts.reviewRuns, err = store.ReviewRuns(ctx, jobID, facts.job.Revision)
		if err != nil {
			return facts, err
		}
	} else if !errors.Is(planErr, sql.ErrNoRows) && !errors.Is(planErr, postgres.ErrNotFound) {
		return facts, planErr
	}
	facts.proposal, err = store.Proposal(ctx, jobID)
	if err != nil {
		return facts, err
	}
	return facts, nil
}

func codingPrerequisitesComplete(f currentWorkFacts) bool {
	return actionSucceeded(f.actions, spine.ActionSandboxCreate, f.sandbox.ID) &&
		actionSucceeded(f.actions, spine.ActionRepositoryClone, f.sandbox.ID) &&
		f.setup != nil && f.setup.State == spine.ActionSucceeded &&
		actionSucceeded(f.actions, spine.ActionRouteCreate, f.sandbox.ID)
}

// decideCurrentWork is intentionally an ordinary, coding-specific decision.
// Its order is the dependency chain; do not replace it with a generic graph,
// registry, persisted projection, or database-side workflow interpreter.
func decideCurrentWork(f currentWorkFacts) Work {
	work := func(kind WorkKind, factID, detail string) Work {
		return Work{Kind: kind, Revision: f.job.Revision, FactID: factID, Detail: detail}
	}
	if f.outcome != nil {
		return work(WorkComplete, f.outcome.JobID, string(f.outcome.Kind))
	}
	if !f.job.AdmissionOpen {
		return work(WorkComplete, f.job.ID, "admission closed")
	}

	// Infrastructure is a fixed prerequisite chain owned by the Job's main
	// Sandbox. Missing Actions are created only when RunJob executes this work.
	if !actionSucceeded(f.actions, spine.ActionSandboxCreate, f.sandbox.ID) {
		return work(WorkCreateSandbox, spine.ScopedActionID(f.job.ID, spine.ActionSandboxCreate, f.sandbox.ID), "")
	}
	if !actionSucceeded(f.actions, spine.ActionRepositoryClone, f.sandbox.ID) {
		return work(WorkCloneRepository, spine.ScopedActionID(f.job.ID, spine.ActionRepositoryClone, f.sandbox.ID), "")
	}
	if f.setup == nil {
		return work(WorkSetupRepository, spine.ActionID(f.job.ID, spine.ActionRepositorySetup), "")
	}
	if f.setup.State == spine.ActionFailed {
		return work(WorkAttention, f.setup.ID, attentionDetail(f.job, f.setup.ID, "repository setup failed"))
	}
	if f.setup.State != spine.ActionSucceeded {
		return work(WorkSetupRepository, f.setup.ID, attentionDetail(f.job, f.setup.ID, ""))
	}
	if !actionSucceeded(f.actions, spine.ActionRouteCreate, f.sandbox.ID) {
		return work(WorkCreateRoute, spine.ScopedActionID(f.job.ID, spine.ActionRouteCreate, f.sandbox.ID), "")
	}
	// Once exact-Revision publication Actions exist, reconcile them before a
	// later accepted Message can advance the Revision. The Proposal then makes
	// that old publication unambiguous and becomes stale after the new Revision.
	if f.proposal == nil && publicationPending(f.actions, f.job.Revision) {
		return work(WorkPublishProposal, f.job.Revision, publicationDetail(f, "reconcile started publication"))
	}

	// Once ReviewPolicy selected exact reviewers, complete their independent
	// AgentRuns before consuming the feedback Messages they produce.
	if f.reviewPlan != nil {
		byRole := make(map[string]spine.ReviewRunView, len(f.reviewRuns))
		for _, run := range f.reviewRuns {
			byRole[run.Role] = run
		}
		for _, role := range f.reviewPlan.Plan.Roles {
			run, ok := byRole[string(role)]
			if !ok {
				return work(WorkAttention, f.job.Revision, fmt.Sprintf("selected reviewer %s has no AgentRun", role))
			}
			if run.FeedbackMessageID != "" {
				continue
			}
			if run.State == spine.AgentRunFailed || run.State == spine.AgentRunInterrupted || run.State == spine.AgentRunUncertain {
				return work(WorkAttention, run.ID, attentionDetail(f.job, run.ID, agentRunAttention(run.AgentRun)))
			}
			return work(WorkRunReviewer, run.ID, string(role))
		}
	}

	// Messages share one implementation lane: steers may target the active
	// Turn; turn-starting follows remain FIFO. DeliveryCandidate is read-only.
	if f.delivery != nil {
		run := f.delivery.AgentRun
		if run.State == spine.AgentRunFailed || run.State == spine.AgentRunInterrupted || run.State == spine.AgentRunUncertain {
			return work(WorkAttention, run.ID, attentionDetail(f.job, run.ID, agentRunAttention(run)))
		}
		return work(WorkDeliverMessage, run.ID, fmt.Sprintf("Message %d", f.delivery.Message.Sequence))
	}
	if latest := latestImplementationFollow(f); latest != nil &&
		(latest.State == spine.AgentRunFailed || latest.State == spine.AgentRunInterrupted || latest.State == spine.AgentRunUncertain) {
		return work(WorkAttention, latest.ID, attentionDetail(f.job, latest.ID, agentRunAttention(*latest)))
	}

	// A completed implementation Turn is not a Git fact. Its immutable
	// git-revision Evidence is the recovery boundary, even when HEAD is unchanged.
	if candidate := revisionCandidate(f); candidate != nil {
		if candidate.InputRevision != f.job.Revision {
			return work(WorkAttention, candidate.ID, fmt.Sprintf("AgentRun input Revision %s does not match current Revision %s", candidate.InputRevision, f.job.Revision))
		}
		return work(WorkObserveRevision, candidate.ID, attentionDetail(f.job, candidate.ID, ""))
	}
	if id, detail := unchangedAttention(f); id != "" {
		return work(WorkAttention, id, detail)
	}

	// Checks are exact-Revision facts. A failed Check first creates an ordinary
	// workflow Message; delivery and Git observation above then handle its loop.
	if len(f.declared) == 0 {
		return work(WorkAttention, f.job.Revision, "repository setup declared no deterministic Checks")
	}
	checks := make(map[string]spine.Check)
	for _, check := range f.checks {
		if check.Revision == f.job.Revision {
			checks[check.Name] = check
		}
	}
	for _, declaration := range f.declared {
		check, ok := checks[declaration.Name]
		if !ok || check.State == "running" {
			id := spine.CheckID(f.job.ID, f.job.Revision, declaration.Name)
			return work(WorkRunChecks, id, attentionDetail(f.job, id, declaration.Name))
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
	if f.reviewPlan == nil {
		return work(WorkChooseReview, f.job.Revision, attentionDetail(f.job, spine.ReviewPolicyAttentionSource(f.job.Revision), ""))
	}
	if f.reviewPlan.Revision != f.job.Revision {
		return work(WorkAttention, f.reviewPlan.Revision, "ReviewPlan does not match the current Revision")
	}

	if f.proposal != nil && !f.proposal.Stale && f.proposal.ProposedRevision == f.job.Revision {
		return work(WorkObserveProposal, f.proposal.URL, fmt.Sprintf("pull request #%d", f.proposal.Number))
	}
	return work(WorkPublishProposal, f.job.Revision, "")
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

func publicationDetail(f currentWorkFacts, fallback string) string {
	for _, action := range f.actions {
		if action.Scope == f.job.Revision && action.ID == f.job.WorkflowAttentionSource &&
			(action.Kind == spine.ActionRepositoryPush || action.Kind == spine.ActionGitHubPullRequest) {
			return f.job.WorkflowAttention
		}
	}
	return fallback
}

func attentionDetail(job spine.Job, source, fallback string) string {
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

func messageSequenceByID(messages []spine.MessageView) map[string]int64 {
	sequences := make(map[string]int64, len(messages))
	for _, message := range messages {
		sequences[message.ID] = message.Sequence
	}
	return sequences
}

func revisionCandidate(f currentWorkFacts) *spine.AgentRun {
	observed := make(map[string]bool)
	for _, record := range f.evidence {
		if record.Kind == "git-revision" {
			observed[record.AgentRunID] = true
		}
	}
	latest := latestImplementationFollow(f)
	if latest == nil || latest.State != spine.AgentRunCompleted || observed[latest.ID] {
		return nil
	}
	return latest
}

func latestImplementationFollow(f currentWorkFacts) *spine.AgentRun {
	messages := make(map[string]spine.MessageView, len(f.messages))
	for _, message := range f.messages {
		messages[message.ID] = message
	}
	var latest *spine.AgentRun
	var latestSequence int64
	for i := range f.runs {
		run := &f.runs[i]
		message, ok := messages[run.MessageID]
		if run.Role == "implement" && ok && message.Intent == spine.MessageFollow && message.Sequence >= latestSequence {
			latest, latestSequence = run, message.Sequence
		}
	}
	return latest
}

// unchangedAttention distinguishes an intentionally adjudicated review or
// Proposal feedback batch from an initial request or failed-Check response
// that returned without fixing anything. Equality is already carried by the
// AgentRun input Revision and its git-revision Evidence.
func unchangedAttention(f currentWorkFacts) (string, string) {
	sequences := messageSequenceByID(f.messages)
	runs := make(map[string]spine.AgentRun, len(f.runs))
	implementationMessages := make(map[string]bool)
	for _, run := range f.runs {
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
	for _, record := range f.evidence {
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
	exactProposal := f.proposal != nil && !f.proposal.Stale && f.proposal.ProposedRevision == f.job.Revision
	for _, message := range f.messages {
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
