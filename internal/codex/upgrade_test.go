package codex

import (
	"github.com/aphronio/dorf/internal/core"
	"testing"
)

func TestUpgradeVerificationRequiresExactSettledNativeHistory(t *testing.T) {
	expected := []core.AgentRun{{ThreadID: "thread", TurnID: "turn", TurnOutcome: "completed"}}
	for _, tc := range []struct {
		name  string
		turns []TurnOutcome
		valid bool
	}{
		{"retained", []TurnOutcome{{ID: "turn", Status: "completed"}}, true},
		{"empty", nil, false},
		{"different", []TurnOutcome{{ID: "other", Status: "completed"}}, false},
		{"active", []TurnOutcome{{ID: "turn", Status: "running"}}, false},
		{"changed", []TurnOutcome{{ID: "turn", Status: "failed"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyUpgradeTurns(expected, tc.turns); (err == nil) != tc.valid {
				t.Fatalf("verification error=%v", err)
			}
		})
	}
}
