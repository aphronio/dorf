package spine

import "testing"

func TestAgentRunStateIsTerminal(t *testing.T) {
	tests := map[AgentRunState]bool{
		AgentRunPending: false, AgentRunSubmitting: false, AgentRunActive: false,
		AgentRunCompleted: true, AgentRunFailed: true, AgentRunInterrupted: true,
		AgentRunUncertain: false,
	}
	for state, want := range tests {
		if got := state.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal() = %t, want %t", state, got, want)
		}
	}
}
