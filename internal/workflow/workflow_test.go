package workflow

import "testing"

func TestWakeEventIsStableAndFIFOScoped(t *testing.T) {
	if WakeEvent("job-a", 2) != WakeEvent("job-a", 2) {
		t.Fatal("same admitted FIFO position did not retain its wake identity")
	}
	if WakeEvent("job-a", 2) == WakeEvent("job-b", 2) {
		t.Fatal("distinct Jobs share a wake event")
	}
	if WakeEvent("job-a", 2) == WakeEvent("job-a", 3) {
		t.Fatal("distinct FIFO positions share an immutable Absurd event")
	}
}

func TestCycleCheckpointIsKeyedByImmutableMessageSequence(t *testing.T) {
	if cycleStepName(2) != cycleStepName(2) {
		t.Fatal("same Message sequence did not retain its cycle checkpoint")
	}
	if cycleStepName(2) == cycleStepName(3) {
		t.Fatal("distinct Message sequences share a cycle checkpoint")
	}
}
