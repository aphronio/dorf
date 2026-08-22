package coding

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
)

// ReviewAgentOperation is coding's cohesive strict-review Harness adapter.
// Core owns AgentRun preparation, recovery, submission ordering, observation,
// binding, and fencing; this operation owns only the exact review boundary.
type ReviewAgentOperation struct {
	store     Store
	externals Externals
	job       Job
	run       ReviewRunView
}

func NewReviewAgentOperation(ctx context.Context, store Store, externals Externals, execution core.AgentMessageExecution) (ReviewAgentOperation, error) {
	job, err := store.CodingJob(ctx, execution.Job.ID)
	if err != nil {
		return ReviewAgentOperation{}, err
	}
	run, err := store.ReviewRun(ctx, core.AgentRunID(execution.Message.ID))
	if err != nil {
		return ReviewAgentOperation{}, err
	}
	operation := ReviewAgentOperation{store: store, externals: externals, job: job, run: run}
	if err := operation.validate(ctx, execution); err != nil {
		return ReviewAgentOperation{}, err
	}
	return operation, nil
}

func (s ReviewAgentOperation) Harness() string { return s.externals.Harness() }

func (s ReviewAgentOperation) validate(ctx context.Context, execution core.AgentMessageExecution) error {
	run := s.run
	expectedFromID := ReviewRequestFromID(run.InputRevision, run.Role)
	expectedMessageID := ReviewRequestMessageID(s.job.ID, run.InputRevision, run.Role)
	if execution.Job.ID != s.job.ID || execution.Message.ID != expectedMessageID || execution.AgentRun.ID != run.ID ||
		execution.AgentRun.MessageID != execution.Message.ID || execution.AgentRun.SandboxID != execution.Sandbox.ID ||
		run.MessageID != expectedMessageID || run.Request.ID != expectedMessageID || run.Request.JobID != s.job.ID ||
		run.Request.FromKind != core.MessageFromWorkflow || run.Request.FromID != expectedFromID || run.Request.Intent != core.MessageFollow ||
		run.Request.TargetTurnID != "" || strings.TrimSpace(run.Request.Input) == "" || run.SandboxID != ReviewSandboxName(s.job.ID, run.ID) ||
		run.Sandbox.ID != run.SandboxID || run.Sandbox.JobID != s.job.ID || execution.Sandbox.ID != run.Sandbox.ID ||
		len(run.Sandbox.OwnershipNonce) != 64 || len(run.SubmissionNonce) != 64 || run.Capability != ReviewReadOnlyCapability {
		return reviewBoundaryError("review AgentRun request Message, Sandbox ownership, or exact submission contract is invalid")
	}
	actions, err := s.store.Actions(ctx, s.job.ID)
	if err != nil {
		return err
	}
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, ActionReviewCheckout, core.ActionRouteCreate} {
		expectedID := core.ScopedActionID(s.job.ID, kind, run.Sandbox.ID)
		ready := false
		for _, action := range actions {
			if action.ID == expectedID && action.JobID == s.job.ID && action.Kind == kind && action.Scope == run.Sandbox.ID && action.State == core.ActionSucceeded {
				ready = true
				break
			}
		}
		if !ready {
			return fmt.Errorf("review AgentRun %s requires succeeded %s Action %s", run.ID, kind, expectedID)
		}
	}
	return nil
}

func (s ReviewAgentOperation) attempt(run core.AgentRun) ReviewRunView {
	return reviewRunAttempt(run, s.run.Request, s.run.Sandbox)
}

func (s ReviewAgentOperation) Submit(ctx context.Context, run core.AgentRun, input string) (core.HarnessBinding, error) {
	if input != s.run.Request.Input {
		return core.HarnessBinding{}, reviewBoundaryError("review AgentRun input changed after exact Message validation")
	}
	return s.externals.ReviewInitialTurn(ctx, s.job, s.attempt(run))
}

func (s ReviewAgentOperation) Recover(ctx context.Context, run core.AgentRun) (core.HarnessBinding, error) {
	return s.externals.ReviewRecover(ctx, s.job, s.attempt(run))
}

func (s ReviewAgentOperation) History(ctx context.Context, run core.AgentRun) (core.HarnessHistory, error) {
	if run.ThreadID == "" {
		binding, err := s.Recover(ctx, run)
		if err != nil || binding.Turn.ID == "" {
			return core.HarnessHistory{Harness: binding.Harness, ThreadID: binding.ThreadID}, err
		}
		return core.HarnessHistory{Harness: binding.Harness, ThreadID: binding.ThreadID, Turns: []core.HarnessTurn{binding.Turn}}, nil
	}
	return s.externals.ReviewTurns(ctx, s.job, s.attempt(run))
}

var _ core.AgentRunOperation = ReviewAgentOperation{}
