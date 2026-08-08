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

func TestPublicationProofBarriersRequireIssue43Phrase(t *testing.T) {
	for _, point := range []string{spine.BarrierPushAccepted, spine.BarrierPullRequestAccepted, spine.BarrierPublicationBegin, spine.BarrierPublicationSpawn} {
		t.Run(point, func(t *testing.T) {
			t.Setenv("DORF_PROOF_FAULT_BARRIER", point)
			t.Setenv("DORF_PROOF_FAULT_BARRIER_JOB", "job-exact")
			t.Setenv("DORF_PROOF_FAULT_BARRIER_DIR", t.TempDir())
			t.Setenv("DORF_PROOF_FAULT_BARRIER_ENABLE", workflowEnablePhrase)
			if _, err := FromEnv(); err == nil {
				t.Fatal("older workflow proof phrase enabled publication barrier")
			}
			t.Setenv("DORF_PROOF_FAULT_BARRIER_ENABLE", publicationEnablePhrase)
			barrier, err := FromEnv()
			if err != nil || barrier == nil {
				t.Fatalf("barrier=%v err=%v", barrier, err)
			}
		})
	}
}

func TestCleanupProofBarriersRequireIssue39Phrase(t *testing.T) {
	for _, point := range []string{spine.BarrierReviewerRouteRevoked, spine.BarrierReviewerSandboxDeleted, spine.BarrierMainRouteRevoked, spine.BarrierMainSandboxDeleted} {
		t.Run(point, func(t *testing.T) {
			t.Setenv("DORF_PROOF_FAULT_BARRIER", point)
			t.Setenv("DORF_PROOF_FAULT_BARRIER_JOB", "job-exact")
			t.Setenv("DORF_PROOF_FAULT_BARRIER_DIR", t.TempDir())
			t.Setenv("DORF_PROOF_FAULT_BARRIER_ENABLE", publicationEnablePhrase)
			if _, err := FromEnv(); err == nil {
				t.Fatal("publication proof phrase enabled cleanup barrier")
			}
			t.Setenv("DORF_PROOF_FAULT_BARRIER_ENABLE", cleanupEnablePhrase)
			barrier, err := FromEnv()
			if err != nil || barrier == nil {
				t.Fatalf("barrier=%v err=%v", barrier, err)
			}
		})
	}
}

func TestPublicationSchedulingBarrierDoesNotRequireTaskContext(t *testing.T) {
	dir := t.TempDir()
	barrier := Barrier{Point: spine.BarrierPublicationBegin, JobID: "job-exact", Dir: dir, Wait: time.Second}
	identity := "publication:job-exact:revision:0"
	release := filepath.Join(dir, "job-exact-"+identity+"-"+spine.BarrierPublicationBegin+".release")
	if err := os.WriteFile(release, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := barrier.ReachWorkflow(context.Background(), spine.BarrierPublicationBegin, "job-exact", identity); err != nil {
		t.Fatalf("scheduling barrier required an Absurd task context: %v", err)
	}
}

func TestProofBarrierRecoveryAcceptsOnlyExactBoundedPayload(t *testing.T) {
	dir := t.TempDir()
	barrier := Barrier{Point: spine.BarrierCommitCreated, JobID: "job-exact", Dir: dir, Wait: time.Second, Lease: 2 * time.Second}
	payload := "job=job-exact\nidentity=action-exact\npoint=commit-created-before-record\n"
	ready := filepath.Join(dir, "job-exact-action-exact-commit-created-before-record.ready")
	if recovered, err := recoverReady(ready, payload); err != nil || recovered {
		t.Fatalf("fresh marker recovered=%v err=%v", recovered, err)
	}
	if err := os.WriteFile(ready, []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := barrier.ReachWorkflow(context.Background(), spine.BarrierCommitCreated, "job-exact", "action-exact"); err != nil {
		t.Fatalf("exact recovery failed: %v", err)
	}
	if err := os.WriteFile(ready, []byte(payload+"corrupt=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := barrier.ReachWorkflow(context.Background(), spine.BarrierCommitCreated, "job-exact", "action-exact"); err == nil || !strings.Contains(err.Error(), "conflicts with exact bounded payload") {
		t.Fatalf("mismatch error=%v", err)
	}
}
