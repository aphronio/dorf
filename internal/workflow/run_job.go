package workflow

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type FactStepResultV1 struct {
	FactID string `json:"fact_id"`
}

func actionStepName(id string) string   { return "dorf/action/v1/" + id }
func setupStepName(id string) string    { return "dorf/setup/v1/" + id }
func agentRunStepName(id string) string { return "dorf/agent-run/v1/" + id }
func revisionStepName(id string) string { return "dorf/revision/v1/" + id }
func checkStepName(id string) string    { return "dorf/check/v1/" + id }
func reviewPolicyStepName(job, revision string) string {
	return fmt.Sprintf("dorf/review-policy/v1/%s/%s", job, revision)
}

// RunJob tells the coding story in dependency order. CurrentWork derives the
// next operation from product facts; this loop executes it and asks again.
// PostgreSQL never stores this disposable answer.
func RunJob(ctx context.Context, service spine.Service, store postgres.Store, proposal ProposalRuntime, jobID string) (Work, error) {
	if service.Repository == nil {
		return Work{}, fmt.Errorf("coding workflow requires repository externals")
	}
	for {
		work, err := CurrentWork(ctx, store, proposal.Publication, jobID)
		if err != nil {
			return Work{}, err
		}
		if work.Kind == WorkComplete || work.Kind == WorkAttention {
			return work, nil
		}

		job, err := store.Job(ctx, jobID)
		if err != nil {
			return Work{}, err
		}
		sandbox, err := store.Sandbox(ctx, spine.MainSandboxName(jobID))
		if err != nil {
			return Work{}, err
		}

		switch work.Kind {
		case WorkCreateSandbox:
			err = runSandboxAction(ctx, service, store, job, sandbox, spine.ActionSandboxCreate)
		case WorkCloneRepository:
			err = runSandboxAction(ctx, service, store, job, sandbox, spine.ActionRepositoryClone)
		case WorkSetupRepository:
			err = runSetupStep(ctx, service, store, job, work)
		case WorkCreateRoute:
			err = runSandboxAction(ctx, service, store, job, sandbox, spine.ActionRouteCreate)
		case WorkRunReviewer:
			err = runFactStep(ctx, agentRunStepName(work.FactID), work.FactID, func(workCtx context.Context) error {
				return service.RunReview(workCtx, job, work.FactID)
			})
		case WorkDeliverMessage:
			err = runDeliveryStep(ctx, service, store, job, work)
		case WorkObserveRevision:
			err = runRevisionStep(ctx, service, store, job, work)
		case WorkRunChecks:
			err = runCheckSteps(ctx, service, store, job)
		case WorkChooseReview:
			err = runFactStep(ctx, reviewPolicyStepName(job.ID, job.Revision), job.Revision, func(workCtx context.Context) error {
				return service.PlanReview(workCtx, job)
			})
		case WorkPublishProposal:
			err = runPublicationSteps(ctx, store, proposal, job)
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

func runSetupStep(ctx context.Context, service spine.Service, store postgres.Store, job spine.Job, work Work) error {
	setup, err := store.BeginSetup(ctx, job.ID)
	if err != nil {
		return err
	}
	if setup.ID != work.FactID {
		return fmt.Errorf("selected setup Action changed from %s to %s", work.FactID, setup.ID)
	}
	if setup.State == spine.ActionSucceeded || setup.State == spine.ActionFailed {
		return nil
	}
	return runFactStep(ctx, setupStepName(setup.ID), setup.ID, func(workCtx context.Context) error {
		return service.ExecuteSetup(workCtx, job, setup)
	})
}

func runDeliveryStep(ctx context.Context, service spine.Service, store postgres.Store, job spine.Job, work Work) error {
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
	return runFactStep(ctx, agentRunStepName(delivery.AgentRun.ID), delivery.AgentRun.ID, func(workCtx context.Context) error {
		return service.Deliver(workCtx, job, *delivery)
	})
}

func runRevisionStep(ctx context.Context, service spine.Service, store postgres.Store, job spine.Job, work Work) error {
	run, ready, err := store.RevisionCandidate(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	if run.ID != work.FactID {
		return fmt.Errorf("Revision observation candidate changed from AgentRun %s to %s", work.FactID, run.ID)
	}
	return runFactStep(ctx, revisionStepName(run.ID), run.ID, func(workCtx context.Context) error {
		return service.ObserveRevision(workCtx, job, run)
	})
}

func runPublicationSteps(ctx context.Context, store postgres.Store, proposal ProposalRuntime, job spine.Job) error {
	_, push, pull, err := store.BeginPublication(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	if push.State != spine.ActionSucceeded {
		if err := runFactStep(ctx, actionStepName(push.ID), push.ID, func(workCtx context.Context) error {
			return proposal.Publication.Push(workCtx, job.ID, job.Revision)
		}); err != nil {
			return fmt.Errorf("publish exact Revision: %w", err)
		}
	}
	if pull.State != spine.ActionSucceeded {
		if err := runFactStep(ctx, actionStepName(pull.ID), pull.ID, func(workCtx context.Context) error {
			return proposal.Publication.Propose(workCtx, job.ID, job.Revision)
		}); err != nil {
			return fmt.Errorf("propose exact Revision: %w", err)
		}
	}
	return nil
}

func runSandboxAction(ctx context.Context, service spine.Service, store postgres.Store, job spine.Job, sandbox spine.Sandbox, kind spine.ActionKind) error {
	action, err := store.GetOrCreateSandboxAction(ctx, sandbox.ID, kind)
	if err != nil {
		return err
	}
	if action.State == spine.ActionSucceeded {
		return nil
	}
	return runFactStep(ctx, actionStepName(action.ID), action.ID, func(workCtx context.Context) error {
		return service.ExecuteSandboxAction(workCtx, job, sandbox, action)
	})
}

func runCheckSteps(ctx context.Context, service spine.Service, store postgres.Store, job spine.Job) error {
	declared, err := store.DeclaredChecks(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, declaration := range declared {
		check, err := store.BeginCheck(ctx, job.ID, job.Revision, declaration.Name, declaration.Command)
		if err != nil {
			return err
		}
		if check.State == "passed" {
			continue
		}
		if err := runFactStep(ctx, checkStepName(check.ID), check.ID, func(workCtx context.Context) error {
			return service.ExecuteCheck(workCtx, job, check)
		}); err != nil {
			return err
		}
		// A failed Check has now admitted feedback as a normal Message. Let the
		// shared decision choose delivery before any later Check.
		return nil
	}
	return nil
}

func runFactStep(ctx context.Context, name, factID string, work func(context.Context) error) error {
	result, err := absurd.Step(ctx, name, func(stepCtx context.Context) (FactStepResultV1, error) {
		return absurdruntime.WithHeartbeat(stepCtx, func(workCtx context.Context) (FactStepResultV1, error) {
			if err := work(workCtx); err != nil {
				return FactStepResultV1{}, err
			}
			return FactStepResultV1{FactID: factID}, nil
		})
	})
	if err != nil {
		return err
	}
	if result.FactID != factID {
		return fmt.Errorf("Step %s returned fact %q, want %q", name, result.FactID, factID)
	}
	return nil
}
