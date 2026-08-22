package terminal

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// AgentRunOperation binds ordinary Harness translation to one authoritative
// Job and Sandbox. Core decides whether the durable run requires initial
// submission, follow submission, recovery, or observation.
type AgentRunOperation struct {
	externals Externals
	job       core.Job
	sandbox   core.Sandbox
}

func NewAgentRunOperation(externals Externals, execution core.AgentMessageExecution) (AgentRunOperation, error) {
	if execution.Job.ID == "" || execution.Sandbox.ID == "" || execution.Sandbox.JobID != execution.Job.ID ||
		execution.AgentRun.JobID != execution.Job.ID || execution.AgentRun.SandboxID != execution.Sandbox.ID {
		return AgentRunOperation{}, fmt.Errorf("ordinary Agent operation requires the exact Job-owned Sandbox")
	}
	return AgentRunOperation{externals: externals, job: execution.Job, sandbox: execution.Sandbox}, nil
}

func (o AgentRunOperation) Harness() string { return o.externals.Agent.Name() }

func (o AgentRunOperation) Submit(ctx context.Context, run core.AgentRun, input string) (core.HarnessBinding, error) {
	owner, err := o.owner(ctx, run)
	if err != nil {
		return core.HarnessBinding{}, err
	}
	if run.ThreadID == "" {
		return o.externals.Agent.StartInitialTurn(ctx, owner, o.externals.Sandbox.Workspace(), run.ID, input, o.job.Model, o.job.ReasoningEffort)
	}
	return o.externals.Agent.StartTurn(ctx, owner, o.externals.Sandbox.Workspace(), run.ThreadID, run.ID, input, o.job.Model, o.job.ReasoningEffort)
}

func (o AgentRunOperation) Recover(ctx context.Context, run core.AgentRun) (core.HarnessBinding, error) {
	owner, err := o.owner(ctx, run)
	if err != nil {
		return core.HarnessBinding{}, err
	}
	history, err := o.externals.Agent.ReadInitialTurns(ctx, owner, o.externals.Sandbox.Workspace())
	if err != nil || history.ThreadID == "" || len(history.Turns) == 0 {
		return core.HarnessBinding{}, err
	}
	return core.HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: history.Turns[len(history.Turns)-1]}, nil
}

func (o AgentRunOperation) History(ctx context.Context, run core.AgentRun) (core.HarnessHistory, error) {
	owner, err := o.owner(ctx, run)
	if err != nil {
		return core.HarnessHistory{}, err
	}
	if run.ThreadID == "" {
		return o.externals.Agent.ReadInitialTurns(ctx, owner, o.externals.Sandbox.Workspace())
	}
	return o.externals.Agent.ReadTurns(ctx, owner, run.ThreadID)
}

func (o AgentRunOperation) owner(ctx context.Context, run core.AgentRun) (provider.Ownership, error) {
	if run.JobID != o.job.ID || run.SandboxID != o.sandbox.ID {
		return provider.Ownership{}, fmt.Errorf("AgentRun %s changed its exact Job or Sandbox binding", run.ID)
	}
	return o.externals.owner(ctx, o.sandbox.ID)
}
