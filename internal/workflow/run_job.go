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
func verifyStepName(job, revision string) string {
	return fmt.Sprintf("dorf/checks-verified/v1/%s/%s", job, revision)
}

// RunJob tells the coding story in order. PostgreSQL owns the facts; each
// external operation gets one stable Absurd Step derived from its Dorf fact.
func RunJob(ctx context.Context, client *absurd.Client, service spine.Service, store postgres.Store, jobID string) (spine.RunDisposition, error) {
	if service.Repository == nil {
		return spine.RunIdle, fmt.Errorf("coding workflow requires repository externals")
	}
	for _, kind := range []spine.ActionKind{spine.ActionSandboxCreate, spine.ActionRepositoryClone} {
		job, err := store.Job(ctx, jobID)
		if err != nil {
			return spine.RunIdle, err
		}
		if !job.AdmissionOpen {
			return spine.RunClosed, nil
		}
		action, err := store.BeginAction(ctx, jobID, kind)
		if err != nil {
			return spine.RunIdle, err
		}
		if err := runActionStep(ctx, service, job, action); err != nil {
			return spine.RunIdle, fmt.Errorf("reconcile %s: %w", kind, err)
		}
	}

	job, err := store.Job(ctx, jobID)
	if err != nil {
		return spine.RunIdle, err
	}
	setup, err := store.BeginSetup(ctx, jobID)
	if err != nil {
		return spine.RunIdle, err
	}
	switch setup.State {
	case spine.ActionSucceeded:
	case spine.ActionFailed:
		return spine.RunBlocked, nil
	default:
		if err := runFactStep(ctx, setupStepName(setup.ID), setup.ID, func(workCtx context.Context) error {
			return service.ExecuteSetup(workCtx, job, setup)
		}); err != nil {
			return spine.RunIdle, err
		}
	}

	job, err = store.Job(ctx, jobID)
	if err != nil {
		return spine.RunIdle, err
	}
	if job.WorkflowPhase == "blocked" {
		return spine.RunBlocked, nil
	}
	route, err := store.BeginAction(ctx, jobID, spine.ActionRouteCreate)
	if err != nil {
		return spine.RunIdle, err
	}
	if err := runActionStep(ctx, service, job, route); err != nil {
		return spine.RunIdle, fmt.Errorf("reconcile %s: %w", route.Kind, err)
	}

	for {
		job, err = store.Job(ctx, jobID)
		if err != nil {
			return spine.RunIdle, err
		}
		if !job.AdmissionOpen {
			return spine.RunClosed, nil
		}
		switch job.WorkflowPhase {
		case "blocked":
			return spine.RunBlocked, nil
		case "ready", "publishing", "publication-blocked", "published":
			if err := continuePublication(ctx, store, client, service.Barrier, job); err != nil {
				return spine.RunIdle, err
			}
			return spine.RunIdle, nil
		case "review-planning":
			if err := runFactStep(ctx, reviewPolicyStepName(job.ID, job.Revision), job.Revision, func(workCtx context.Context) error {
				return service.PlanReview(workCtx, job)
			}); err != nil {
				return spine.RunIdle, err
			}
			continue
		case "reviewing":
			if err := runReviewSteps(ctx, service, store, job); err != nil {
				return spine.RunIdle, err
			}
			continue
		}

		if job.SessionID == "" {
			if err := runInitialAgentStep(ctx, service, store, job); err != nil {
				return spine.RunIdle, err
			}
			continue
		}

		delivery, err := store.NextDelivery(ctx, jobID, job.SessionID)
		if err != nil {
			return spine.RunIdle, err
		}
		if delivery != nil {
			switch delivery.AgentRun.State {
			case spine.AgentRunUncertain:
				return spine.RunBlocked, nil
			case spine.AgentRunFailed, spine.AgentRunInterrupted:
				if delivery.AgentRun.NativeTurnID != "" {
					return spine.RunBlocked, nil
				}
			}
			if err := runFactStep(ctx, agentRunStepName(delivery.AgentRun.ID), delivery.AgentRun.ID, func(workCtx context.Context) error {
				_, err := service.Deliver(workCtx, job, *delivery)
				return err
			}); err != nil {
				return spine.RunIdle, err
			}
			continue
		}

		switch job.WorkflowPhase {
		case "implementing", "review-feedback":
			run, ready, err := store.RevisionCandidate(ctx, jobID, job.Revision)
			if err != nil {
				return spine.RunIdle, err
			}
			if !ready {
				return spine.RunIdle, nil
			}
			if err := runFactStep(ctx, revisionStepName(run.ID), run.ID, func(workCtx context.Context) error {
				return service.ObserveRevision(workCtx, job, run)
			}); err != nil {
				return spine.RunIdle, err
			}
		case "checking":
			if err := runCheckSteps(ctx, service, store, job); err != nil {
				return spine.RunIdle, err
			}
		default:
			return spine.RunIdle, fmt.Errorf("unsupported coding workflow phase %q", job.WorkflowPhase)
		}
	}
}

func runActionStep(ctx context.Context, service spine.Service, job spine.Job, action spine.Action) error {
	if action.State == spine.ActionSucceeded {
		return nil
	}
	return runFactStep(ctx, actionStepName(action.ID), action.ID, func(workCtx context.Context) error {
		_, err := service.ExecuteAction(workCtx, job, action)
		return err
	})
}

func runInitialAgentStep(ctx context.Context, service spine.Service, store postgres.Store, job spine.Job) error {
	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		return err
	}
	delivery, err := store.NextDelivery(ctx, job.ID, "")
	if err != nil {
		return err
	}
	if delivery == nil || delivery.Message.Sequence != 1 || delivery.Message.Intent != spine.MessageFollow {
		return fmt.Errorf("unbound native Session has no initial delivery")
	}
	if delivery.AgentRun.State == spine.AgentRunUncertain {
		return fmt.Errorf("initial AgentRun %s is uncertain: %s", delivery.AgentRun.ID, delivery.AgentRun.Attention)
	}
	return runFactStep(ctx, agentRunStepName(delivery.AgentRun.ID), delivery.AgentRun.ID, func(workCtx context.Context) error {
		_, err := service.DeliverInitial(workCtx, job, session, *delivery)
		return err
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
		if check.State != "passed" {
			if err := runFactStep(ctx, checkStepName(check.ID), check.ID, func(workCtx context.Context) error {
				return service.ExecuteCheck(workCtx, job, check)
			}); err != nil {
				return err
			}
			current, err := store.Job(ctx, job.ID)
			if err != nil {
				return err
			}
			if current.WorkflowPhase != "checking" {
				return nil
			}
		}
	}
	return runFactStep(ctx, verifyStepName(job.ID, job.Revision), job.Revision, func(workCtx context.Context) error {
		return service.VerifyChecks(workCtx, job, declared)
	})
}

func runFactStep(ctx context.Context, name, factID string, work func(context.Context) error) error {
	result, err := absurd.Step(ctx, name, func(stepCtx context.Context) (FactStepResultV1, error) {
		return absurdruntime.WithHeartbeat(stepCtx, func(workCtx context.Context) (FactStepResultV1, error) {
			return FactStepResultV1{FactID: factID}, work(workCtx)
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

func runReviewSteps(ctx context.Context, service spine.Service, store postgres.Store, job spine.Job) error {
	plan, err := store.ReviewPlan(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	runs, err := store.ReviewRuns(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	byRole := make(map[string]spine.ReviewRunView, len(runs))
	for _, run := range runs {
		byRole[run.Role] = run
	}
	for _, role := range plan.Plan.Roles {
		run, ok := byRole[string(role)]
		if !ok {
			return fmt.Errorf("selected review Role %s has no AgentRun", role)
		}
		if run.FeedbackMessageID != "" {
			continue
		}
		return runFactStep(ctx, agentRunStepName(run.ID), run.ID, func(workCtx context.Context) error {
			return service.RunReview(workCtx, job, run.ID)
		})
	}
	return fmt.Errorf("reviewing Revision %s has no pending reviewer", job.Revision)
}
