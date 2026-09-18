package direct

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
)

// InspectionStore is the complete durable input for one direct Session snapshot.
// Task execution, cleanup policy, and presentation remain outside this seam.
type InspectionStore interface {
	Sandboxes(context.Context, string) ([]core.Sandbox, error)
	Actions(context.Context, string) ([]core.Action, error)
	Deliveries(context.Context, string) ([]core.Delivery, error)
}

// Snapshot is one factual read of a direct Session and the resources it owns.
type Snapshot struct {
	Session     core.Session
	MainSandbox core.Sandbox
	Sandboxes   []core.Sandbox
	Actions     []core.Action
	Deliveries  []core.Delivery
}

// LoadSnapshot performs one staged load and fails closed when any fact does
// not belong to the exact direct Session contract.
func LoadSnapshot(ctx context.Context, store InspectionStore, session core.Session) (Snapshot, error) {
	snapshot := Snapshot{Session: session}
	if session.ID == "" {
		return Snapshot{}, fmt.Errorf("direct Session identity is empty")
	}
	if session.Workflow != "" || session.WorkflowRevision != "" {
		return Snapshot{}, fmt.Errorf("Session %s is not direct", session.ID)
	}

	var err error
	snapshot.Sandboxes, err = store.Sandboxes(ctx, session.ID)
	if err != nil {
		return Snapshot{}, err
	}
	mainID := core.MainSandboxName(session.ID)
	for _, sandbox := range snapshot.Sandboxes {
		if sandbox.SessionID != session.ID {
			return Snapshot{}, fmt.Errorf("Sandbox %s does not belong to direct Session %s", sandbox.ID, session.ID)
		}
		if sandbox.ID == mainID && sandbox.Name == core.DefaultSandbox {
			snapshot.MainSandbox = sandbox
		}
	}
	if snapshot.MainSandbox.ID == "" {
		return Snapshot{}, fmt.Errorf("direct Session %s has no exact default Sandbox reservation", session.ID)
	}

	snapshot.Actions, err = store.Actions(ctx, session.ID)
	if err != nil {
		return Snapshot{}, err
	}
	for _, action := range snapshot.Actions {
		if action.SessionID != session.ID {
			return Snapshot{}, fmt.Errorf("Action %s does not belong to direct Session %s", action.ID, session.ID)
		}
	}

	snapshot.Deliveries, err = store.Deliveries(ctx, session.ID)
	if err != nil {
		return Snapshot{}, err
	}
	for _, delivery := range snapshot.Deliveries {
		message, run := delivery.Message, delivery.AgentRun
		if message.SessionID != session.ID || run.SessionID != session.ID || run.MessageID != message.ID ||
			run.Role != DirectAgentRole || run.SandboxID != mainID {
			return Snapshot{}, fmt.Errorf("Message %s does not have an exact direct delivery for Session %s", message.ID, session.ID)
		}
	}
	return snapshot, nil
}

// ExecutionState is a disposable projection of current direct execution.
// Admission and cleanup are independent Session facts and do not change it.
type ExecutionState string

const (
	ExecutionProvisioningSandbox ExecutionState = "provisioning-sandbox"
	ExecutionConnectingRoute     ExecutionState = "connecting-route"
	ExecutionQueued              ExecutionState = "queued"
	ExecutionWorking             ExecutionState = "working"
	ExecutionAttention           ExecutionState = "attention"
	ExecutionIdle                ExecutionState = "idle"
)

type Projection struct {
	State  ExecutionState
	Detail string
}

// Project derives current direct execution without reading or mutating state.
func (s Snapshot) Project() Projection {
	if s.Session.WorkflowAttention != "" {
		return Projection{State: ExecutionAttention, Detail: s.Session.WorkflowAttention}
	}
	if !core.HasSucceededAction(s.Actions, core.ActionSandboxCreate, s.MainSandbox.ID) {
		return Projection{State: ExecutionProvisioningSandbox}
	}
	if !core.HasSucceededAction(s.Actions, core.ActionRouteCreate, s.MainSandbox.ID) {
		return Projection{State: ExecutionConnectingRoute}
	}
	state := ExecutionIdle
	for _, delivery := range s.Deliveries {
		run := delivery.AgentRun
		switch run.State {
		case core.AgentRunCompleted, core.AgentRunFailed, core.AgentRunInterrupted:
			continue
		}
		if run.Attention != "" {
			return Projection{State: ExecutionAttention, Detail: run.Attention}
		}
		switch run.State {
		case core.AgentRunUncertain:
			return Projection{State: ExecutionAttention, Detail: "agent delivery ended with state " + string(run.State)}
		case core.AgentRunActive:
			state = ExecutionWorking
		default:
			if state != ExecutionWorking {
				state = ExecutionQueued
			}
		}
	}
	if state != ExecutionIdle {
		return Projection{State: state}
	}
	return latestSettledProjection(s.Deliveries)
}

func latestSettledProjection(deliveries []core.Delivery) Projection {
	var latest *core.Delivery
	for _, delivery := range deliveries {
		if delivery.Message.Intent == core.MessageSteer && delivery.AgentRun.State == core.AgentRunCompleted && delivery.AgentRun.TurnOutcome == "" {
			continue
		}
		if latest == nil || delivery.Message.Sequence > latest.Message.Sequence {
			latest = &delivery
		}
	}
	if latest == nil {
		return Projection{State: ExecutionIdle}
	}
	run := latest.AgentRun
	if run.Attention != "" {
		return Projection{State: ExecutionAttention, Detail: run.Attention}
	}
	if run.State != core.AgentRunCompleted {
		return Projection{State: ExecutionAttention, Detail: "agent delivery ended with state " + string(run.State)}
	}
	if run.TurnOutcome != "completed" {
		return Projection{State: ExecutionAttention, Detail: "agent completed without a successful Turn outcome"}
	}
	return Projection{State: ExecutionIdle}
}
