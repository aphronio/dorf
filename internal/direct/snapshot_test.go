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
		{name: "Job attention", change: func(s *Snapshot) { s.Job.WorkflowAttention = "job needs intervention"; s.Actions = nil }, state: ExecutionAttention, detail: "job needs intervention"},
		{name: "provisioning Sandbox", change: func(s *Snapshot) { s.Actions = nil }, state: ExecutionProvisioningSandbox},
		{name: "connecting route", change: func(s *Snapshot) { s.Actions = s.Actions[:1] }, state: ExecutionConnectingRoute},
		{name: "working Agent", change: func(s *Snapshot) { s.Deliveries[0].AgentRun = core.AgentRun{State: core.AgentRunActive} }, state: ExecutionWorking},
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

func TestSnapshotProjectsOutstandingWorkBeforeSettledHistory(t *testing.T) {
	pending := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunPending}}
	submitting := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunSubmitting}}
	active := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunActive}}
	success := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunCompleted, TurnOutcome: "completed"}}
	failed := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunFailed, Attention: "delivery failed"}}
	interrupted := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunInterrupted}}
	uncertain := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunUncertain}}
	attention := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunActive, Attention: "needs intervention"}}
	unsuccessful := core.Delivery{AgentRun: core.AgentRun{State: core.AgentRunCompleted, TurnOutcome: "failed"}}
	steerACK := core.Delivery{Message: core.Message{Intent: core.MessageSteer}, AgentRun: core.AgentRun{State: core.AgentRunCompleted}}
	tests := []struct {
		name       string
		deliveries []core.Delivery
		state      ExecutionState
	}{
		{"no messages", nil, ExecutionIdle},
		{"pending", []core.Delivery{pending}, ExecutionQueued},
		{"submitting", []core.Delivery{submitting}, ExecutionQueued},
		{"active", []core.Delivery{active}, ExecutionWorking},
		{"active before pending", []core.Delivery{active, pending}, ExecutionWorking},
		{"active after pending", []core.Delivery{pending, active}, ExecutionWorking},
		{"active before steer acknowledgement", []core.Delivery{active, steerACK}, ExecutionWorking},
		{"active after steer acknowledgement", []core.Delivery{steerACK, active}, ExecutionWorking},
		{"failed then pending", []core.Delivery{failed, pending}, ExecutionQueued},
		{"failed then submitting", []core.Delivery{failed, submitting}, ExecutionQueued},
		{"failed then active", []core.Delivery{failed, active}, ExecutionWorking},
		{"failed then success", []core.Delivery{failed, success}, ExecutionIdle},
		{"interrupted then pending", []core.Delivery{interrupted, pending}, ExecutionQueued},
		{"interrupted then success", []core.Delivery{interrupted, success}, ExecutionIdle},
		{"unsuccessful then success", []core.Delivery{unsuccessful, success}, ExecutionIdle},
		{"success then failed", []core.Delivery{success, failed}, ExecutionAttention},
		{"success then unsuccessful", []core.Delivery{success, unsuccessful}, ExecutionAttention},
		{"uncertain before active", []core.Delivery{uncertain, active}, ExecutionAttention},
		{"uncertain after active", []core.Delivery{active, uncertain}, ExecutionAttention},
		{"active with attention", []core.Delivery{attention, pending}, ExecutionAttention},
		{"attention after active", []core.Delivery{active, attention}, ExecutionAttention},
		{"success before acknowledgement", []core.Delivery{success, steerACK}, ExecutionIdle},
		{"failure before acknowledgement", []core.Delivery{failed, steerACK}, ExecutionAttention},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for i := range test.deliveries {
				test.deliveries[i].Message.Sequence = int64(i + 1)
			}
			snapshot := Snapshot{
				MainSandbox: core.Sandbox{ID: "main"},
				Actions: []core.Action{
					{Kind: core.ActionSandboxCreate, Scope: "main", State: core.ActionSucceeded},
					{Kind: core.ActionRouteCreate, Scope: "main", State: core.ActionSucceeded},
				},
				Deliveries: test.deliveries,
			}
			if got := snapshot.Project(); got.State != test.state {
				t.Fatalf("Project() = %#v, want %s", got, test.state)
			}
		})
	}
}
