package proofbarrier

import (
	"context"
	"os"
	"path/filepath"
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
	t.Setenv("DORF_PROOF_FAULT_BARRIER_ENABLE", proofEnablePhrase)
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

func TestProofBarrierRecoveryAcceptsOnlyExactBoundedPayload(t *testing.T) {
	dir := t.TempDir()
	barrier := Barrier{Point: spine.BarrierSetupComplete, JobID: "job-exact", Dir: dir, Wait: time.Second, Lease: 2 * time.Second}
	payload := "job=job-exact\nidentity=action-exact\npoint=setup-complete-before-record\n"
	ready := filepath.Join(dir, "job-exact-action-exact-setup-complete-before-record.ready")
	if recovered, err := recoverReady(ready, payload); err != nil || recovered {
		t.Fatalf("fresh marker recovered=%v err=%v", recovered, err)
	}
	if err := os.WriteFile(ready, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := barrier.ReachWorkflow(context.Background(), spine.BarrierSetupComplete, "job-exact", "action-exact"); err != nil {
		t.Fatalf("exact recovery failed: %v", err)
	}
	if err := os.WriteFile(ready, []byte(payload+"corrupt=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := barrier.ReachWorkflow(context.Background(), spine.BarrierSetupComplete, "job-exact", "action-exact"); err == nil || !strings.Contains(err.Error(), "conflicts with exact bounded payload") {
		t.Fatalf("mismatch error=%v", err)
	}
}
