package direct

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
)

// InspectionStore is the complete durable input for one direct Job snapshot.
// Task execution, cleanup policy, and presentation remain outside this seam.
type InspectionStore interface {
	Sandboxes(context.Context, string) ([]core.Sandbox, error)
	Actions(context.Context, string) ([]core.Action, error)
	Deliveries(context.Context, string) ([]core.Delivery, error)
}

// Snapshot is one factual read of a direct Job and the resources it owns.
type Snapshot struct {
	Job         core.Job
	MainSandbox core.Sandbox
	Sandboxes   []core.Sandbox
	Actions     []core.Action
	Deliveries  []core.Delivery
}

// LoadSnapshot performs one staged load and fails closed when any fact does
// not belong to the exact direct Job contract.
func LoadSnapshot(ctx context.Context, store InspectionStore, job core.Job) (Snapshot, error) {
	snapshot := Snapshot{Job: job}
	if job.ID == "" {
		return Snapshot{}, fmt.Errorf("direct Job identity is empty")
	}
	if job.Workflow != "" || job.WorkflowRevision != "" {
		return Snapshot{}, fmt.Errorf("Job %s is not direct", job.ID)
	}

	var err error
	snapshot.Sandboxes, err = store.Sandboxes(ctx, job.ID)
	if err != nil {
		return Snapshot{}, err
	}
	mainID := core.MainSandboxName(job.ID)
	for _, sandbox := range snapshot.Sandboxes {
		if sandbox.JobID != job.ID {
			return Snapshot{}, fmt.Errorf("Sandbox %s does not belong to direct Job %s", sandbox.ID, job.ID)
		}
		if sandbox.ID == mainID && sandbox.Name == core.DefaultSandbox {
			snapshot.MainSandbox = sandbox
		}
	}
	if snapshot.MainSandbox.ID == "" {
		return Snapshot{}, fmt.Errorf("direct Job %s has no exact default Sandbox reservation", job.ID)
	}

	snapshot.Actions, err = store.Actions(ctx, job.ID)
	if err != nil {
		return Snapshot{}, err
	}
	for _, action := range snapshot.Actions {
		if action.JobID != job.ID {
			return Snapshot{}, fmt.Errorf("Action %s does not belong to direct Job %s", action.ID, job.ID)
		}
	}

	snapshot.Deliveries, err = store.Deliveries(ctx, job.ID)
	if err != nil {
		return Snapshot{}, err
	}
	for _, delivery := range snapshot.Deliveries {
		message, run := delivery.Message, delivery.AgentRun
		if message.JobID != job.ID || run.JobID != job.ID || run.MessageID != message.ID ||
			run.Role != DirectAgentRole || run.SandboxID != mainID {
			return Snapshot{}, fmt.Errorf("Message %s does not have an exact direct delivery for Job %s", message.ID, job.ID)
		}
	}
	return snapshot, nil
}

// ExecutionState is a disposable projection of current direct execution.
// Admission and cleanup are independent Job facts and do not change it.
type ExecutionState string

const (
	ExecutionProvisioningSandbox ExecutionState = "provisioning-sandbox"
	ExecutionConnectingRoute     ExecutionState = "connecting-route"
	ExecutionAwaitingAgent       ExecutionState = "awaiting-agent"
	ExecutionAttention           ExecutionState = "attention"
	ExecutionIdle                ExecutionState = "idle"
)

type Projection struct {
	State  ExecutionState
	Detail string
}

// Project derives current direct execution without reading or mutating state.
func (s Snapshot) Project() Projection {
	if s.Job.WorkflowAttention != "" {
		return Projection{State: ExecutionAttention, Detail: s.Job.WorkflowAttention}
	}
	if !core.HasSucceededAction(s.Actions, core.ActionSandboxCreate, s.MainSandbox.ID) {
		return Projection{State: ExecutionProvisioningSandbox}
	}
	if !core.HasSucceededAction(s.Actions, core.ActionRouteCreate, s.MainSandbox.ID) {
		return Projection{State: ExecutionConnectingRoute}
	}
	for _, delivery := range s.Deliveries {
		run := delivery.AgentRun
		if run.Attention != "" {
			return Projection{State: ExecutionAttention, Detail: run.Attention}
		}
		switch run.State {
		case core.AgentRunCompleted:
			if run.TurnOutcome == "completed" {
				continue
			}
			return Projection{State: ExecutionAttention, Detail: "agent completed without a successful Turn outcome"}
		case core.AgentRunFailed, core.AgentRunInterrupted, core.AgentRunUncertain:
			return Projection{State: ExecutionAttention, Detail: "agent delivery ended with state " + string(run.State)}
		default:
			return Projection{State: ExecutionAwaitingAgent}
		}
	}
	return Projection{State: ExecutionIdle}
}
