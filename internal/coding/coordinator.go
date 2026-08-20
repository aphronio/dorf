package coding

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

// CodingExecution is the coding workflow's explicit composition of reusable
// Core execution with only its own Revision and review policy.
type CodingExecution interface {
	gitworkspace.Execution
	BlobStore() blob.Store
	ObserveRevision(context.Context, Job, core.AgentRun) error
	PlanReview(context.Context, Job) error
	RunReview(context.Context, Job, string) error
	ExecuteReviewCheckout(context.Context, Job, string, core.Action) error
}

func agentRunStepName(id string) string { return "dorf/agent-run/v1/" + id }
func revisionStepName(id string) string { return "dorf/revision/v1/" + id }
func reviewPolicyStepName(job, revision string) string {
	return fmt.Sprintf("dorf/review-policy/v1/%s/%s", job, revision)
}

// RunJob tells the coding story in dependency order. CurrentWork derives the
// next operation from product facts; this loop executes it and asks again.
// PostgreSQL never stores this disposable answer.
func RunJob(ctx context.Context, service CodingExecution, store Store, proposal ProposalRuntime, jobID string) (Work, error) {
	for {
		snapshot, err := LoadSnapshot(ctx, store, jobID)
		if err != nil {
			return Work{}, err
		}
		projection, err := snapshot.Project(service.BlobStore())
		if err != nil {
			return Work{}, err
		}
		work := projection.CurrentWork
		if work.Kind == WorkComplete || work.Kind == WorkAttention {
			return work, nil
		}

		job := snapshot.Job

		switch work.Kind {
		case WorkAction:
			err = runSandboxAction(ctx, service, store, job, snapshot, work)
		case WorkRunReviewer:
			err = absurdruntime.RunFactStep(ctx, agentRunStepName(work.FactID), work.FactID, func(workCtx context.Context) error {
				return service.RunReview(workCtx, job, work.FactID)
			})
		case WorkDeliverMessage:
			err = runDeliveryStep(ctx, service, store, job, work)
		case WorkObserveAgent:
			terminal, observeErr := observeAgentRun(ctx, service, job, snapshot, work)
			if observeErr != nil {
				return Work{}, observeErr
			}
			if terminal {
				continue
			}
			return work, nil
		case WorkObserveRevision:
			err = runRevisionStep(ctx, service, job, snapshot, work)
		case WorkChooseReview:
			err = absurdruntime.RunFactStep(ctx, reviewPolicyStepName(job.ID, job.Revision), job.Revision, func(workCtx context.Context) error {
				return service.PlanReview(workCtx, job)
			})
		case WorkPublishProposal:
			err = runPublicationStep(ctx, store, proposal, job)
		case WorkObserveProposal:
			observation, observeErr := observeProposal(ctx, proposal, job.ID, job.Revision)
			if observeErr != nil {
				return Work{}, observeErr
			}
			if observation.Outcome != "" || observation.NewMessages > 0 {
				continue
			}
			return work, nil
		default:
			return Work{}, fmt.Errorf("unsupported current coding work %q", work.Kind)
		}
		if err != nil {
			return Work{}, err
		}
	}
}

func observeAgentRun(ctx context.Context, service CodingExecution, job Job, snapshot Snapshot, work Work) (bool, error) {
	for i := range snapshot.Deliveries {
		run := snapshot.Deliveries[i].AgentRun
		if run.ID != work.FactID {
			continue
		}
		if run.JobID != job.ID || run.Role != "implement" || run.State != core.AgentRunActive {
			return false, fmt.Errorf("AgentRun observation changed from exact active implementation AgentRun %s", work.FactID)
		}
		turn, err := service.ObserveAgentRunTurn(ctx, job.Job, run, "implement")
		if err != nil {
			return false, err
		}
		return turn.Terminal(), nil
	}
	return false, fmt.Errorf("AgentRun observation has no exact implementation AgentRun %s", work.FactID)
}

func runDeliveryStep(ctx context.Context, service CodingExecution, store Store, job Job, work Work) error {
	delivery, err := store.NextDelivery(ctx, job.ID)
	if err != nil {
		return err
	}
	if delivery == nil {
		return nil
	}
	if delivery.AgentRun.ID != work.FactID {
		return fmt.Errorf("delivery candidate changed from AgentRun %s to %s", work.FactID, delivery.AgentRun.ID)
	}
	return absurdruntime.RunFactStep(ctx, agentRunStepName(delivery.AgentRun.ID), delivery.AgentRun.ID, func(workCtx context.Context) error {
		return service.Deliver(workCtx, job.Job, *delivery, codingAgentInput(job, *delivery))
	})
}

func codingAgentInput(job Job, delivery core.Delivery) string {
	if delivery.AgentRun.Role != "implement" {
		return delivery.Message.Input
	}
	return fmt.Sprintf("%s\n\nDorf coding workflow contract: work on branch %q from accepted Revision %s. Before returning control, commit every intended workspace change. You may create one commit or several. Leave the checkout clean, with final HEAD on that branch and descending from the accepted Revision. If this input explicitly concludes that no code change is warranted, leave HEAD unchanged and the checkout clean.", strings.TrimSpace(delivery.Message.Input), job.Branch, job.Revision)
}

func runRevisionStep(ctx context.Context, service CodingExecution, job Job, snapshot Snapshot, work Work) error {
	var run *core.AgentRun
	for i := range snapshot.Deliveries {
		if snapshot.Deliveries[i].AgentRun.ID == work.FactID {
			run = &snapshot.Deliveries[i].AgentRun
			break
		}
	}
	if run == nil || run.JobID != job.ID || run.Role != "implement" || run.InputRevision != job.Revision {
		return fmt.Errorf("Revision observation has no exact current implementation AgentRun %s", work.FactID)
	}
	return absurdruntime.RunFactStep(ctx, revisionStepName(run.ID), run.ID, func(workCtx context.Context) error {
		return service.ObserveRevision(workCtx, job, *run)
	})
}

func runPublicationStep(ctx context.Context, store Store, proposal ProposalRuntime, job Job) error {
	_, push, pull, err := store.BeginPublication(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	if push.State != core.ActionSucceeded {
		return absurdruntime.RunActionStep(ctx, push.ID, func(workCtx context.Context) error {
			return proposal.Publication.Push(workCtx, job.ID, job.Revision)
		})
	}
	if pull.State != core.ActionSucceeded {
		return absurdruntime.RunActionStep(ctx, pull.ID, func(workCtx context.Context) error {
			return proposal.Publication.Propose(workCtx, job.ID, job.Revision)
		})
	}
	return nil
}

func runSandboxAction(ctx context.Context, service CodingExecution, store Store, job Job, snapshot Snapshot, work Work) error {
	var sandbox *core.Sandbox
	for i := range snapshot.Sandboxes {
		if snapshot.Sandboxes[i].ID == work.Scope {
			sandbox = &snapshot.Sandboxes[i]
			break
		}
	}
	if sandbox == nil || sandbox.JobID != job.ID {
		return fmt.Errorf("Action %s has no exact Job-owned Sandbox %s", work.FactID, work.Scope)
	}

	var reviewer *ReviewRunView
	if sandbox.ID != snapshot.MainSandbox.ID {
		reviewRuns, err := snapshot.currentReviewRuns()
		if err != nil {
			return err
		}
		for i := range reviewRuns {
			run := &reviewRuns[i]
			if run.InputRevision == job.Revision && run.Sandbox.ID == sandbox.ID {
				reviewer = run
				break
			}
		}
		if reviewer == nil {
			return fmt.Errorf("review Action %s has no selected reviewer Sandbox %s", work.FactID, work.Scope)
		}
	}

	expectedID := core.ScopedActionID(job.ID, work.ActionKind, work.Scope)
	if expectedID != work.FactID {
		return fmt.Errorf("Action changed from %s to %s", work.FactID, expectedID)
	}
	action, err := store.GetOrCreateSandboxAction(ctx, sandbox.ID, work.ActionKind)
	if err != nil {
		return err
	}
	if action.ID != work.FactID || action.JobID != job.ID || action.Kind != work.ActionKind || action.Scope != work.Scope {
		return fmt.Errorf("selected Action %s changed to %s %s in %s", work.FactID, action.ID, action.Kind, action.Scope)
	}
	if action.State == core.ActionSucceeded {
		return nil
	}
	return absurdruntime.RunActionStep(ctx, action.ID, func(workCtx context.Context) error {
		if work.ActionKind == ActionReviewCheckout {
			if reviewer == nil {
				return fmt.Errorf("review checkout Action %s belongs to the main Sandbox", action.ID)
			}
			return service.ExecuteReviewCheckout(workCtx, job, reviewer.ID, action)
		}
		if work.ActionKind == gitworkspace.ActionRepositoryClone {
			return service.ExecuteRepositoryClone(workCtx, job.Job, *sandbox, action, job.Repository, job.Revision, job.Branch)
		}
		return service.ExecuteSandboxAction(workCtx, job.Job, *sandbox, action)
	})
}
