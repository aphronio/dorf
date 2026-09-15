package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestProfilePromotionSerializesAdmission(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := fmt.Sprintf("promote-serial-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, completeIncusProfile(name, "codex", strings.Repeat("a", 64))); err != nil {
		t.Fatal(err)
	}
	verify := func() core.ProfileVerification {
		t.Helper()
		_, proof, err := store.BeginSandboxProfileVerification(ctx, name)
		if err == nil {
			err = store.RecordSandboxProfileProbe(ctx, proof, "codex test")
		}
		if err == nil {
			err = store.RecordSandboxProfileVerificationCleanup(ctx, proof)
		}
		if err != nil {
			t.Fatal(err)
		}
		return proof
	}
	old := verify()
	image := strings.Repeat("b", 64)
	if _, _, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{IncusArtifact: &image}); err != nil {
		t.Fatal(err)
	}
	next := verify()
	// Hold the same non-key pointer UPDATE used by promotion. Admission's snapshot
	// starts with A and must select B after the transaction releases its lock.
	if _, err := db.ExecContext(ctx, `update dorf.sandbox_profiles set active_revision=$2 where name=$1`, name, old.DefinitionHash); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `update dorf.sandbox_profiles set active_revision=$2 where name=$1`, name, next.DefinitionHash); err != nil {
		t.Fatal(err)
	}
	type result struct {
		job core.Job
		err error
	}
	done := make(chan result, 1)
	go func() {
		job, _, err := store.AdmitDirect(ctx, core.JobAdmission{AdmissionKey: name, SandboxProfile: name, ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "high"}, client.QueueName())
		done <- result{job, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("admission crossed promotion lock: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.job.SandboxProfileRevision != next.DefinitionHash {
		t.Fatalf("admission after promotion=%+v err=%v", got.job, got.err)
	}
}

func TestProfileStagingUsesCandidateAfterConcurrentUpdate(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := fmt.Sprintf("stage-serial-%d", time.Now().UnixNano())
	old, _, err := store.CreateSandboxProfile(ctx, completeIncusProfile(name, "codex", strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	image := strings.Repeat("b", 64)
	next, _, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{IncusArtifact: &image})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.sandbox_profiles set candidate_revision=$2 where name=$1`, name, old.DefinitionHash); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `update dorf.sandbox_profiles set candidate_revision=$2 where name=$1`, name, next.DefinitionHash); err != nil {
		t.Fatal(err)
	}
	type result struct {
		profile core.SandboxProfile
		err     error
	}
	done := make(chan result, 1)
	gateway := "http://10.45.0.1:8317/v1"
	go func() {
		profile, _, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{IncusGatewayURL: &gateway})
		done <- result{profile, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("staging crossed candidate lock: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.profile.Artifact != image || got.profile.IncusGatewayURL != gateway {
		t.Fatalf("concurrent patch=%+v err=%v", got.profile, got.err)
	}
}
