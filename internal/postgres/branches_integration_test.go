package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
)

func TestCheckpointBranchOwnsHeldDestinationAndLeavesSourceIndependent(t *testing.T) {
	db, store, tasks := testDatabase(t)
	ctx := context.Background()
	source, sourceSandboxID, boundary := completedCheckpointFixture(t, store, ctx, "branch")
	reference := checkpointReference("branch-repository", source.ID)
	checkpoint, err := store.PublishCheckpoint(ctx, boundary, reference)
	if err != nil {
		t.Fatal(err)
	}
	// The source may continue after publication. Branching still selects the
	// immutable native revision and never holds or rewinds the source.
	advanceNativeFixture(t, store, ctx, source.ID, source.ThreadID, "later-source-input")
	request := persistence.BranchRequest{ID: fmt.Sprintf("branch-%d", time.Now().UnixNano()),
		SourceSessionID: source.ID, Repository: reference.Repository, SnapshotID: reference.SnapshotID}
	branch, err := store.RequestCheckpointBranch(ctx, tasks.QueueName(), request)
	if err != nil {
		t.Fatal(err)
	}
	otherRequest := request
	otherRequest.ID += "-other"
	other, err := store.RequestCheckpointBranch(ctx, tasks.QueueName(), otherRequest)
	if err != nil || other.DestinationSessionID == branch.DestinationSessionID || other.DestinationSandboxID == branch.DestinationSandboxID {
		t.Fatalf("second branch reused destination custody: %#v / %v", other, err)
	}
	replay, err := store.RequestCheckpointBranch(ctx, tasks.QueueName(), request)
	if err != nil || replay != branch {
		t.Fatalf("branch replay changed receipt: %#v / %v", replay, err)
	}
	changed := request
	changed.SnapshotID = checkpointReference(reference.Repository, "other").SnapshotID
	if _, err := store.RequestCheckpointBranch(ctx, tasks.QueueName(), changed); err == nil {
		t.Fatal("conflicting branch identity was accepted")
	}
	if branch.DestinationSessionID == source.ID || branch.DestinationSandboxID == sourceSandboxID ||
		branch.Checkpoint != checkpoint || branch.ThreadID != source.ThreadID {
		t.Fatalf("branch did not retain exact independent custody: %#v", branch)
	}
	destination, err := store.Session(ctx, branch.DestinationSessionID)
	if err != nil || destination.ThreadID != source.ThreadID || destination.CurrentTaskID == "" ||
		destination.SandboxProfileRevision != source.SandboxProfileRevision {
		t.Fatalf("destination Session was not atomically admitted: %#v / %v", destination, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, branch.DestinationSandboxID); err != nil || !held {
		t.Fatalf("destination was not held: %t / %v", held, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, sourceSandboxID); err != nil || held {
		t.Fatalf("source was held: %t / %v", held, err)
	}
	if _, err := store.BeginNativeMutation(ctx, branch.DestinationSessionID, branch.ThreadID, "blocked", ""); err == nil {
		t.Fatal("held branch admitted native work")
	}
	if revision, err := store.BeginNativeMutation(ctx, source.ID, source.ThreadID, "source-continues", ""); err != nil {
		t.Fatalf("source could not continue: %v", err)
	} else if err := store.FinishNativeMutation(ctx, source.ID, revision); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.CheckpointBranchFilePreparationAllowed(ctx, branch.DestinationSandboxID); err != nil || allowed {
		t.Fatalf("pre-restore file preparation was allowed: %t / %v", allowed, err)
	}
	if _, err := store.ReleaseCheckpointBranch(ctx, tasks.QueueName(), branch.ID); err == nil {
		t.Fatal("unrestored branch was released")
	}
	if err := store.RecordCheckpointBranchRestored(ctx, branch.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.CheckpointBranchFilePreparationAllowed(ctx, branch.DestinationSandboxID); err != nil || !allowed {
		t.Fatalf("restored branch file preparation was denied: %t / %v", allowed, err)
	}
	if _, err := store.ReleaseCheckpointBranch(ctx, tasks.QueueName(), branch.ID); err != nil {
		t.Fatal(err)
	}
	if allowed, err := store.CheckpointBranchFilePreparationAllowed(ctx, branch.DestinationSandboxID); err != nil || allowed {
		t.Fatalf("file preparation remained open during release: %t / %v", allowed, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, branch.DestinationSandboxID); err != nil || !held {
		t.Fatalf("release request dropped hold before verification: %t / %v", held, err)
	}
	if err := store.FinishCheckpointBranch(ctx, branch); err == nil {
		t.Fatal("unprovisioned branch was marked ready")
	}
	owned, err := store.Sandbox(ctx, branch.DestinationSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSandboxResource(ctx, owned, "provider-branch"); err != nil {
		t.Fatal(err)
	}
	routeID := core.ScopedActionID(branch.DestinationSessionID, core.ActionRouteCreate, branch.DestinationSandboxID)
	if _, err := db.ExecContext(ctx, `insert into dorf.actions(id,session_id,kind,state,scope_key,settled_at)
values($1,$2,'provider-route-create','succeeded',$3,clock_timestamp())`, routeID, branch.DestinationSessionID, branch.DestinationSandboxID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishCheckpointBranch(ctx, branch); err != nil {
		t.Fatal(err)
	}
	ready, err := store.CheckpointBranch(ctx, branch.ID)
	if err != nil || ready.ReadyAt.IsZero() {
		t.Fatalf("branch was not ready: %#v / %v", ready, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, branch.DestinationSandboxID); err != nil || held {
		t.Fatalf("verified branch stayed held: %t / %v", held, err)
	}
	if err := store.FinishCheckpointBranch(ctx, branch); err != nil {
		t.Fatalf("ready acknowledgement replay failed: %v", err)
	}
	if branchBoundary, err := store.Boundary(ctx, branch.DestinationSandboxID, false); err != nil ||
		branchBoundary.SessionID != branch.DestinationSessionID || branchBoundary.SandboxID != branch.DestinationSandboxID ||
		branchBoundary.NativeRevision != checkpoint.NativeRevision {
		t.Fatalf("future backups would not use destination custody: %#v / %v", branchBoundary, err)
	}
	if err := requestCleanupFixture(ctx, store, branch.DestinationSessionID); err != nil {
		t.Fatal(err)
	}
	cleanupTask := "cleanup-branch-" + branch.ID
	if err := attachTaskFixture(store, ctx, branch.DestinationSessionID, cleanupTask, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxResourceDeleted(ctx, owned); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []core.ActionKind{core.ActionRouteRevoke, core.ActionSandboxDelete} {
		action, err := store.GetOrCreateSandboxAction(ctx, branch.DestinationSandboxID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CompleteCleanup(ctx, branch.DestinationSessionID, cleanupTask); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Session(ctx, branch.DestinationSessionID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("destination cleanup did not finish: %#v / %v", cleaned, err)
	}
	stillOwned, err := store.Sandbox(ctx, sourceSandboxID)
	if err != nil || stillOwned.ProviderID != "provider-original" {
		t.Fatalf("destination cleanup affected source resource: %#v / %v", stillOwned, err)
	}
	sourceAfter, err := store.Session(ctx, source.ID)
	if err != nil || !sourceAfter.AdmissionOpen || sourceAfter.CleanupState != core.CleanupPending {
		t.Fatalf("destination cleanup affected source Session: %#v / %v", sourceAfter, err)
	}
	otherAfter, err := store.Session(ctx, other.DestinationSessionID)
	if err != nil || !otherAfter.AdmissionOpen || otherAfter.CleanupState != core.CleanupPending {
		t.Fatalf("destination cleanup affected sibling branch: %#v / %v", otherAfter, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, other.DestinationSandboxID); err != nil || !held {
		t.Fatalf("sibling branch lost its hold: %t / %v", held, err)
	}
}

func TestCheckpointBranchRejectsPackageGenerationWithoutDestinationCustody(t *testing.T) {
	db, store, tasks := testDatabase(t)
	ctx := context.Background()
	source, sandboxID, boundary := completedCheckpointFixture(t, store, ctx, "branch-package")
	upgradeID := fmt.Sprintf("upgrade-branch-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason,released_at)
values($1,$2,'workspace_upgrade',clock_timestamp())`, upgradeID, sandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_upgrades(
id,sandbox_id,source_resource_id,package_path,version,previous_version,
quiesced_at,checkpoint_key,checkpoint_reference,checkpoint_source_id,
activated_at,verified_at,checkpoint_deleted_at,finished_at)
values($1,$2,$3,'/nix/store/synthetic-package','1.2.3','1.2.2',
clock_timestamp(),$1,'provider-checkpoint','provider-original',
clock_timestamp(),clock_timestamp(),clock_timestamp(),clock_timestamp())`, upgradeID, sandboxID, boundary.ResourceID); err != nil {
		t.Fatal(err)
	}
	boundary, err := store.Boundary(ctx, sandboxID, false)
	if err != nil || boundary.EffectiveUpgradeID != upgradeID {
		t.Fatalf("source package boundary: %#v / %v", boundary, err)
	}
	reference := checkpointReference("branch-repository", source.ID+":package")
	if _, err := store.PublishCheckpoint(ctx, boundary, reference); err != nil {
		t.Fatal(err)
	}
	request := persistence.BranchRequest{ID: fmt.Sprintf("branch-package-%d", time.Now().UnixNano()),
		SourceSessionID: source.ID, Repository: reference.Repository, SnapshotID: reference.SnapshotID}
	if _, err := store.RequestCheckpointBranch(ctx, tasks.QueueName(), request); err == nil {
		t.Fatal("branch admitted a package generation without destination-owned package custody")
	}
	if exists, err := store.SessionExists(ctx, core.SessionID("checkpoint-branch:"+request.ID)); err != nil || exists {
		t.Fatalf("rejected package branch allocated a Session: %t / %v", exists, err)
	}
}
