package direct

import (
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestSnapshotProjectsDirectExecutionIndependently(t *testing.T) {
	sessionID := "session-direct"
	main := core.Sandbox{ID: core.MainSandboxName(sessionID), SessionID: sessionID, Name: core.DefaultSandbox}
	succeeded := func(kind core.ActionKind) core.Action {
		return core.Action{SessionID: sessionID, Kind: kind, Scope: main.ID, State: core.ActionSucceeded}
	}
	ready := Snapshot{
		Session:     core.Session{ID: sessionID, AdmissionOpen: true, CleanupState: core.CleanupPending},
		MainSandbox: main,
		Actions:     []core.Action{succeeded(core.ActionSandboxCreate), succeeded(core.ActionRouteCreate)},
	}

	tests := []struct {
		name   string
		change func(*Snapshot)
		state  ExecutionState
		detail string
	}{
		{name: "Session attention", change: func(s *Snapshot) { s.Session.ExecutionAttention = "session needs intervention"; s.Actions = nil }, state: ExecutionAttention, detail: "session needs intervention"},
		{name: "provisioning Sandbox", change: func(s *Snapshot) { s.Actions = nil }, state: ExecutionProvisioningSandbox},
		{name: "connecting route", change: func(s *Snapshot) { s.Actions = s.Actions[:1] }, state: ExecutionConnectingRoute},
		{name: "open idle", change: func(*Snapshot) {}, state: ExecutionIdle},
		{
			name: "closed and cleaned remains execution idle",
			change: func(s *Snapshot) {
				s.Session.AdmissionOpen = false
				s.Session.CleanupState = core.CleanupComplete
				s.Session.CleanupAttention = "cleanup detail is independent"
			},
			state: ExecutionIdle,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := ready
			snapshot.Actions = append([]core.Action(nil), ready.Actions...)
			test.change(&snapshot)
			projection := snapshot.Project()
			if projection.State != test.state || projection.Detail != test.detail {
				t.Fatalf("Project() = %#v, want state %q and detail %q", projection, test.state, test.detail)
			}
		})
	}
}
