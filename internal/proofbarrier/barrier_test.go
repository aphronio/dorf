package proofbarrier

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/spine"
)

func TestProofBarrierIsDisabledByDefaultAndRequiresExplicitPhrase(t *testing.T) {
	t.Setenv("DORF_PROOF_FAULT_BARRIER", "")
	barrier, err := FromEnv()
	if err != nil || barrier != nil {
		t.Fatalf("default barrier=%v err=%v", barrier, err)
	}
	t.Setenv("DORF_PROOF_FAULT_BARRIER", spine.BarrierBeforeSubmit)
	t.Setenv("DORF_PROOF_FAULT_BARRIER_SEQUENCE", "1")
	t.Setenv("DORF_PROOF_FAULT_BARRIER_DIR", t.TempDir())
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "proof-only enable phrase") {
		t.Fatalf("unsafe activation error=%v", err)
	}
	t.Setenv("DORF_PROOF_FAULT_BARRIER_ENABLE", messageEnablePhrase)
	if barrier, err := FromEnv(); err != nil || barrier == nil {
		t.Fatalf("explicit proof barrier=%v err=%v", barrier, err)
	}
}

func TestProofBarrierRejectsTimingThatCouldOutliveItsClaim(t *testing.T) {
	barrier := Barrier{Point: spine.BarrierBeforeSubmit, Sequence: 1, Dir: t.TempDir(), Wait: 2 * time.Second, Lease: time.Second}
	delivery := spine.Delivery{Message: spine.Message{JobID: "job-proof", Sequence: 1}}
	err := barrier.Reach(context.Background(), spine.BarrierBeforeSubmit, delivery)
	if err == nil || !strings.Contains(err.Error(), "unsafe proof barrier timing") {
		t.Fatalf("timing error=%v", err)
	}
}

func TestRepositoryProofBarrierRequiresIssue37PhraseAndExactJob(t *testing.T) {
	t.Setenv("DORF_PROOF_FAULT_BARRIER", spine.BarrierCommitCreated)
	t.Setenv("DORF_PROOF_FAULT_BARRIER_DIR", t.TempDir())
	t.Setenv("DORF_PROOF_FAULT_BARRIER_ENABLE", workflowEnablePhrase)
	if _, err := FromEnv(); err == nil || !strings.Contains(err.Error(), "BARRIER_JOB") {
		t.Fatalf("missing Job error=%v", err)
	}
	t.Setenv("DORF_PROOF_FAULT_BARRIER_JOB", "job-exact")
	barrier, err := FromEnv()
	if err != nil || barrier == nil {
		t.Fatalf("repository barrier=%v err=%v", barrier, err)
	}
}
