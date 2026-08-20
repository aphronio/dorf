package core

import (
	"encoding/json"
	"testing"
)

func TestMessageWakeContractIsStableAndFIFOScoped(t *testing.T) {
	if MessageWakeEvent("job-a", 2) != MessageWakeEvent("job-a", 2) {
		t.Fatal("same admitted FIFO position did not retain its wake identity")
	}
	if MessageWakeEvent("job-a", 2) == MessageWakeEvent("job-b", 2) || MessageWakeEvent("job-a", 2) == MessageWakeEvent("job-a", 3) {
		t.Fatal("distinct Job FIFO positions share an immutable Absurd event")
	}
	encoded, err := json.Marshal(MessageWakeV1{JobID: "job-1", Sequence: 2})
	if err != nil || string(encoded) != `{"job_id":"job-1","sequence":2}` {
		t.Fatalf("persisted wake JSON = %s, want exact v1 shape: %v", encoded, err)
	}
}
