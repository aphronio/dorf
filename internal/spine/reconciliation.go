package spine

import "fmt"

func ReconcileTurns(baselineRecorded bool, baselineTurnID, knownTurnID string, turns []HarnessTurn) Reconciliation {
	if knownTurnID != "" {
		for _, turn := range turns {
			if turn.ID == knownTurnID {
				return classifyTurn(turn)
			}
		}
		return Reconciliation{Classification: "uncertain", Reason: "bound harness turn is missing from thread history"}
	}
	if !baselineRecorded {
		return Reconciliation{Classification: "uncertain", Reason: "harness turn baseline was not durably recorded"}
	}
	baselineIndex := -1
	if baselineTurnID != "" {
		for i, turn := range turns {
			if turn.ID == baselineTurnID {
				baselineIndex = i
				break
			}
		}
		if baselineIndex < 0 {
			return Reconciliation{Classification: "uncertain", Reason: "durable harness turn baseline is missing from thread history"}
		}
	}
	suffix := turns[baselineIndex+1:]
	if len(suffix) == 0 {
		return Reconciliation{Classification: "no-submit"}
	}
	if len(suffix) > 1 {
		return Reconciliation{Classification: "uncertain", Reason: fmt.Sprintf("%d harness turns appeared after the durable baseline", len(suffix))}
	}
	return classifyTurn(suffix[0])
}

func classifyTurn(turn HarnessTurn) Reconciliation {
	if terminalHarness(turn.Status) {
		return Reconciliation{Classification: turn.Status, Turn: turn}
	}
	if activeHarness(turn.Status) {
		return Reconciliation{Classification: "active", Turn: turn}
	}
	return Reconciliation{Classification: "uncertain", Turn: turn, Reason: fmt.Sprintf("harness turn %s has unsupported status %q", turn.ID, turn.Status)}
}

func ReconcileSteer(clientMessageID, targetTurnID string, turns []HarnessTurn) Reconciliation {
	for _, turn := range turns {
		if turn.ID != targetTurnID {
			continue
		}
		for _, accepted := range turn.AcceptedMessageIDs {
			if accepted == clientMessageID {
				return Reconciliation{Classification: "completed", Turn: turn}
			}
		}
		if terminalHarness(turn.Status) {
			return Reconciliation{Classification: "target-terminal", Turn: turn}
		}
		if !activeHarness(turn.Status) {
			return Reconciliation{Classification: "uncertain", Turn: turn, Reason: fmt.Sprintf("steer target harness turn %s has unsupported status %q", targetTurnID, turn.Status)}
		}
		return Reconciliation{Classification: "no-submit", Turn: turn}
	}
	return Reconciliation{Classification: "uncertain", Reason: fmt.Sprintf("steer target harness turn %s is missing from thread history", targetTurnID)}
}
