package core

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/earendil-works/absurd/sdks/go/absurd"
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

func TestMessageWakeTimeoutReloadsAndForeignPayloadIsRejected(t *testing.T) {
	if err := resolveMessageWake("job-1", 2, MessageWakeV1{}, &absurd.TimeoutError{}); err != nil {
		t.Fatalf("timeout did not request a durable-fact reload: %v", err)
	}
	foreign := MessageWakeV1{JobID: "job-2", Sequence: 3}
	if err := resolveMessageWake("job-1", 2, foreign, nil); err == nil || !strings.Contains(err.Error(), "conflicts with Job job-1 sequence 2") {
		t.Fatalf("foreign wake error=%v", err)
	}
	want := errors.New("await failed")
	if err := resolveMessageWake("job-1", 2, MessageWakeV1{}, want); !errors.Is(err, want) {
		t.Fatalf("await failure=%v, want %v", err, want)
	}
}
