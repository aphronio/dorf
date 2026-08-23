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
	ObserveRevision(context.Context, Job, string) error
	PlanReview(context.Context, Job) error
	RecordReviewResult(context.Context, Job, string) error
	ExecuteReviewCheckout(context.Context, Job, string) error
}

func revisionStepName(id string) string { return "dorf/revision/v1/" + id }
func reviewPolicyStepName(job, revision string) string {
	return fmt.Sprintf("dorf/review-policy/v1/%s/%s", job, revision)
}

// RunJob tells the coding story in dependency order. CurrentWork derives the
// next operation from product facts; this loop executes it and asks again.
// PostgreSQL never stores this disposable answer.
func RunJob(ctx context.Context, custody core.JobHandle, service CodingExecution, store Store, proposal ProposalRuntime, jobID string) (Work, error) {
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
			err = runSandboxAction(ctx, custody, service, job, snapshot, work)
		case WorkRecordReview:
			err = absurdruntime.RunFactStep(ctx, "dorf/review-feedback/v1/"+work.FactID, work.FactID, func(workCtx context.Context) error {
				return service.RecordReviewResult(workCtx, job, work.FactID)
			})
		case WorkWaitAgent:
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
			if attentionNeeded(err) {
				return Work{Kind: WorkAttention, Revision: job.Revision, FactID: work.FactID, Detail: err.Error()}, nil
			}
			return work, err
		}
	}
}

// AgentPrompt is coding-owned prompt policy selected by the deployment's
// static Agent strategy composition after Core reloads the durable Message.
func AgentPrompt(job Job, input string) string {
	return fmt.Sprintf("%s\n\nDorf coding workflow contract: work on branch %q from accepted Revision %s. Before returning control, commit every intended workspace change. You may create one commit or several. Leave the checkout clean, with final HEAD on that branch and descending from the accepted Revision. If this input explicitly concludes that no code change is warranted, leave HEAD unchanged and the checkout clean.", strings.TrimSpace(input), job.Branch, job.Revision)
}

func runRevisionStep(ctx context.Context, service CodingExecution, job Job, _ Snapshot, work Work) error {
	return absurdruntime.RunFactStep(ctx, revisionStepName(work.FactID), work.FactID, func(workCtx context.Context) error {
		return service.ObserveRevision(workCtx, job, work.FactID)
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

func runSandboxAction(ctx context.Context, custody core.JobHandle, service CodingExecution, job Job, snapshot Snapshot, work Work) error {
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
	// Sandbox-create is a truthful projection of the next Core custody
	// prerequisite. Provider mutation remains exclusive to the opaque handle.
	if work.ActionKind == core.ActionSandboxCreate {
		var ensured core.SandboxHandle
		var err error
		if sandbox.Name == core.DefaultSandbox {
			ensured, err = custody.EnsureDefaultSandbox(ctx)
		} else {
			ensured, err = custody.EnsureNamedSandbox(ctx, sandbox.Name)
		}
		if err != nil {
			return err
		}
		if ensured.ID() != sandbox.ID {
			return fmt.Errorf("ensured Sandbox %s changed selected identity %s", ensured.ID(), sandbox.ID)
		}
		return nil
	}
	var reviewer *ReviewRunView
	if sandbox.ID != snapshot.MainSandbox.ID {
		reviewRuns := snapshot.currentReviewRuns()
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

	if expectedID := core.ScopedActionID(job.ID, work.ActionKind, work.Scope); expectedID != work.FactID {
		return fmt.Errorf("Action changed from %s to %s", work.FactID, expectedID)
	}
	if work.ActionKind == ActionReviewCheckout {
		if reviewer == nil {
			return fmt.Errorf("review checkout Action %s belongs to the main Sandbox", work.FactID)
		}
		return service.ExecuteReviewCheckout(ctx, job, reviewer.ID)
	}
	if work.ActionKind == gitworkspace.ActionRepositoryClone {
		return service.ExecuteRepositoryClone(ctx, job.Job, *sandbox, job.Repository, job.Revision, job.Branch)
	}
	return service.ExecuteSandboxAction(ctx, job.ID, sandbox.ID, work.ActionKind)
}
