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
}

// Snapshot is one factual read of a direct Session and the resources it owns.
type Snapshot struct {
	Session     core.Session
	MainSandbox core.Sandbox
	Sandboxes   []core.Sandbox
	Actions     []core.Action
}

// LoadSnapshot performs one staged load and fails closed when any fact does
// not belong to the exact direct Session contract.
func LoadSnapshot(ctx context.Context, store InspectionStore, session core.Session) (Snapshot, error) {
	snapshot := Snapshot{Session: session}
	if session.ID == "" {
		return Snapshot{}, fmt.Errorf("direct Session identity is empty")
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

	return snapshot, nil
}

// ExecutionState is a disposable projection of current direct execution.
// Admission and cleanup are independent Session facts and do not change it.
type ExecutionState string

const (
	ExecutionProvisioningSandbox ExecutionState = "provisioning-sandbox"
	ExecutionConnectingRoute     ExecutionState = "connecting-route"
	ExecutionAttention           ExecutionState = "attention"
	ExecutionIdle                ExecutionState = "idle"
)

type Projection struct {
	State  ExecutionState
	Detail string
}

// Project derives current direct execution without reading or mutating state.
func (s Snapshot) Project() Projection {
	if s.Session.ExecutionAttention != "" {
		return Projection{State: ExecutionAttention, Detail: s.Session.ExecutionAttention}
	}
	if !core.HasSucceededAction(s.Actions, core.ActionSandboxCreate, s.MainSandbox.ID) {
		return Projection{State: ExecutionProvisioningSandbox}
	}
	if !core.HasSucceededAction(s.Actions, core.ActionRouteCreate, s.MainSandbox.ID) {
		return Projection{State: ExecutionConnectingRoute}
	}
	return Projection{State: ExecutionIdle}
}
