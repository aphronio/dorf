package direct

import (
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestSnapshotProjectsDirectExecutionIndependently(t *testing.T) {
	jobID := "job-direct"
	main := core.Sandbox{ID: core.MainSandboxName(jobID), JobID: jobID, Name: core.DefaultSandbox}
	succeeded := func(kind core.ActionKind) core.Action {
		return core.Action{JobID: jobID, Kind: kind, Scope: main.ID, State: core.ActionSucceeded}
	}
	completed := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunCompleted, TurnOutcome: "completed"}}
	ready := Snapshot{
		Job:         core.Job{ID: jobID, AdmissionOpen: true, CleanupState: core.CleanupPending},
		MainSandbox: main,
		Actions:     []core.Action{succeeded(core.ActionSandboxCreate), succeeded(core.ActionRouteCreate)},
		Deliveries:  []core.Delivery{completed},
	}

	tests := []struct {
		name   string
		change func(*Snapshot)
		state  ExecutionState
		detail string
	}{
		{name: "provisioning Sandbox", change: func(s *Snapshot) { s.Actions = nil }, state: ExecutionProvisioningSandbox},
		{name: "connecting route", change: func(s *Snapshot) { s.Actions = s.Actions[:1] }, state: ExecutionConnectingRoute},
		{name: "awaiting Agent", change: func(s *Snapshot) { s.Deliveries[0].AgentRun = core.AgentRun{State: core.AgentRunActive} }, state: ExecutionAwaitingAgent},
		{name: "Agent attention", change: func(s *Snapshot) { s.Deliveries[0].AgentRun = core.AgentRun{State: core.AgentRunFailed} }, state: ExecutionAttention, detail: "agent delivery ended with state failed"},
		{name: "open idle", change: func(*Snapshot) {}, state: ExecutionIdle},
		{
			name: "closed and cleaned remains execution idle",
			change: func(s *Snapshot) {
				s.Job.AdmissionOpen = false
				s.Job.CleanupState = core.CleanupComplete
				s.Job.CleanupAttention = "cleanup detail is independent"
			},
			state: ExecutionIdle,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := ready
			snapshot.Actions = append([]core.Action(nil), ready.Actions...)
			snapshot.Deliveries = append([]core.Delivery(nil), ready.Deliveries...)
			test.change(&snapshot)
			projection := snapshot.Project()
			if projection.State != test.state || projection.Detail != test.detail {
				t.Fatalf("Project() = %#v, want state %q and detail %q", projection, test.state, test.detail)
			}
		})
	}
}
