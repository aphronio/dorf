package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/upgrade"
)

type recoveryDriver struct {
	afterRestore     func()
	restoredSnapshot string
	restoredResource string
	verifiedRuns     []core.AgentRun
	verifiedPackage  persistence.EffectivePackage
	deleted          map[string]bool
}

func (d *recoveryDriver) Restore(_ context.Context, _ core.Session, destination core.Sandbox, checkpoint persistence.Checkpoint) (string, error) {
	d.restoredSnapshot = checkpoint.SnapshotID
	d.restoredResource = destination.ResourceID
	if d.afterRestore != nil {
		d.afterRestore()
	}
	return "provider-replacement", nil
}

func (d *recoveryDriver) VerifyAndRenew(_ context.Context, _ core.Session, destination core.Sandbox, _ persistence.Checkpoint, pkg persistence.EffectivePackage, runs []core.AgentRun) error {
	if destination.ProviderID != "provider-replacement" {
		return errors.New("replacement was not attested")
	}
	d.verifiedRuns = append([]core.AgentRun(nil), runs...)
	d.verifiedPackage = pkg
	return nil
}

func (d *recoveryDriver) DeleteResource(_ context.Context, owned core.Sandbox) error {
	d.deleted[owned.ResourceID] = true
	return nil
}

func TestCheckpointRecoveryReplacesCustodyAndReleasesExactHold(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	session, sandboxID, boundary := completedCheckpointFixture(t, store, ctx, "exact-recovery")
	upgradeID := "upgrade-recovery-" + fmt.Sprint(time.Now().UnixNano())
	packagePath := "/nix/store/" + fmt.Sprintf("%032x", time.Now().UnixNano()) + "-runner-1.2.3"
	if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason,released_at)
values($1,$2,'workspace_upgrade',clock_timestamp())`, upgradeID, sandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_upgrades(
    id,sandbox_id,source_resource_id,package_path,version,previous_version,
    quiesced_at,checkpoint_key,checkpoint_reference,checkpoint_source_id,
    activated_at,verified_at,checkpoint_deleted_at,finished_at
) values(
    $1,$2,$3,$4,'1.2.3','1.2.2',clock_timestamp(),$1,'provider-checkpoint','provider-original',
    clock_timestamp(),clock_timestamp(),clock_timestamp(),clock_timestamp()
)`, upgradeID, sandboxID, boundary.ResourceID, packagePath); err != nil {
		t.Fatal(err)
	}
	var err error
	boundary, err = store.Boundary(ctx, sandboxID, false)
	if err != nil || boundary.EffectiveUpgradeID != upgradeID {
		t.Fatalf("effective package boundary=%#v err=%v", boundary, err)
	}
	reference := persistence.Reference{Repository: "recovery-repository", SnapshotID: checkpointTestID(session.ID + ":exact")}
	checkpoint, err := store.PublishCheckpoint(ctx, boundary, reference)
	if err != nil {
		t.Fatal(err)
	}
	request := persistence.RecoveryRequest{
		ID: "recover-" + session.ID[len(session.ID)-24:], SessionID: session.ID, SandboxID: sandboxID,
		Repository: reference.Repository, SnapshotID: reference.SnapshotID,
	}
	receipt, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), request)
	if err != nil || replay.RequestedAt != receipt.RequestedAt {
		t.Fatalf("request replay: receipt=%#v err=%v", replay, err)
	}
	changed := request
	changed.SnapshotID = checkpointTestID(session.ID + ":changed")
	if _, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), changed); err == nil {
		t.Fatal("changed request reused recovery identity")
	}
	if err := store.ReleaseSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, request.ID); err == nil {
		t.Fatal("generic release bypassed recovery verification")
	}

	queued, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
		SessionID: session.ID, SandboxID: sandboxID, FromKind: core.MessageFromHuman,
		FromID: "queued-after-checkpoint", Input: "continue after recovery", Intent: core.MessageAuto,
	})
	if err != nil || queued.Message.Intent != core.MessageFollow {
		t.Fatalf("queued safe input: result=%#v err=%v", queued, err)
	}
	driver := &recoveryDriver{deleted: map[string]bool{}}
	service := persistence.RecoveryService{
		Store: store, Driver: driver, Queue: client.QueueName(), Claim: func(context.Context) error { return nil },
	}
	// A worker can lose its claim after restore but before binding the provider.
	// Retry must reuse the reserved resource, not allocate another replacement.
	claimErr := errors.New("claim lost after restore")
	var claimFailure error
	service.Claim = func(context.Context) error { return claimFailure }
	driver.afterRestore = func() { claimFailure = claimErr }
	if _, err := service.Reconcile(ctx, session.ID); !errors.Is(err, claimErr) {
		t.Fatalf("lost claim: %v", err)
	}
	partial, err := store.SessionRecoveries(ctx, session.ID)
	if err != nil || partial[0].DestinationProviderID != "" {
		t.Fatalf("stale executor published restore: %#v / %v", partial, err)
	}
	firstDestination := driver.restoredResource
	driver.afterRestore = nil
	claimFailure = nil
	service.Queue = "missing_queue"
	var adoptionErr error
	for range 8 {
		if _, adoptionErr = service.Reconcile(ctx, session.ID); adoptionErr != nil {
			break
		}
	}
	if adoptionErr == nil || !driver.deleted[receipt.SourceResourceID] {
		t.Fatalf("expected atomic adoption to fail after verified source deletion: %v", adoptionErr)
	}
	before, err := store.Sandbox(ctx, sandboxID)
	if err != nil || before.ResourceID != receipt.SourceResourceID {
		t.Fatalf("failed wake partially adopted replacement: sandbox=%#v err=%v", before, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, sandboxID); err != nil || !held {
		t.Fatalf("failed wake released delivery: held=%v err=%v", held, err)
	}
	service.Queue = client.QueueName()
	if _, err := service.Reconcile(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	receipts, err := store.SessionRecoveries(ctx, session.ID)
	if err != nil || len(receipts) != 1 || receipts[0].FinishedAt.IsZero() {
		t.Fatalf("finished recovery receipts=%#v err=%v", receipts, err)
	}
	finished := receipts[0]
	if driver.restoredSnapshot != checkpoint.SnapshotID || driver.restoredResource != finished.DestinationResourceID || driver.restoredResource != firstDestination {
		t.Fatalf("restore did not use exact checkpoint: snapshot=%q resource=%q", driver.restoredSnapshot, driver.restoredResource)
	}
	if len(driver.verifiedRuns) != 1 || driver.verifiedRuns[0].ThreadID != "thread-retained" || driver.verifiedRuns[0].TurnID != "turn-initial" {
		t.Fatalf("native verification prefix=%#v", driver.verifiedRuns)
	}
	if driver.verifiedPackage.UpgradeID != upgradeID || driver.verifiedPackage.PackagePath != packagePath || driver.verifiedPackage.Version != "1.2.3" {
		t.Fatalf("effective package verification=%#v", driver.verifiedPackage)
	}
	if !driver.deleted[finished.SourceResourceID] {
		t.Fatal("source resource deletion was not reconciled")
	}
	active, err := store.Sandbox(ctx, sandboxID)
	if err != nil || active.ResourceID != finished.DestinationResourceID || active.ProviderID != "provider-replacement" {
		t.Fatalf("active replacement=%#v err=%v", active, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, sandboxID); err != nil || held {
		t.Fatalf("verified recovery retained hold: held=%v err=%v", held, err)
	}
	next, err := store.AgentMessage(ctx, session.ID)
	if err != nil || next == nil || next.MessageID != queued.Message.ID {
		t.Fatalf("queued input did not resume: delivery=%#v err=%v", next, err)
	}
	execution, err := store.AgentMessageExecution(ctx, queued.Message.ID)
	if err != nil || execution.Sandbox.ResourceID != finished.DestinationResourceID {
		t.Fatalf("queued input uses stale resource: execution=%#v err=%v", execution, err)
	}
	if _, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), request); err != nil {
		t.Fatal(err)
	}
	if held, _ := store.SandboxDeliveryHeld(ctx, sandboxID); held {
		t.Fatal("replay reopened completed recovery")
	}
}

func TestCheckpointRecoveryHoldsForPostBoundaryNativeSubmission(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	session, sandboxID, boundary := completedCheckpointFixture(t, store, ctx, "unsafe-recovery")
	reference := persistence.Reference{Repository: "unsafe-repository", SnapshotID: checkpointTestID(session.ID + ":unsafe")}
	if _, err := store.PublishCheckpoint(ctx, boundary, reference); err != nil {
		t.Fatal(err)
	}
	request := persistence.RecoveryRequest{
		ID: "recover-unsafe-" + fmt.Sprint(time.Now().UnixNano()), SessionID: session.ID, SandboxID: sandboxID,
		Repository: reference.Repository, SnapshotID: reference.SnapshotID,
	}
	if _, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), request); err != nil {
		t.Fatal(err)
	}
	queued, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
		SessionID: session.ID, SandboxID: sandboxID, FromKind: core.MessageFromHuman,
		FromID: "ambiguous-native", Input: "ambiguous native input", Intent: core.MessageFollow,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Model a lost native submission acknowledgement: no Turn ID exists, but
	// the run has crossed the pristine pending boundary.
	if err := store.PrepareAgentRun(ctx, core.AgentRunID(queued.Message.ID), "codex", "turn-initial"); err != nil {
		t.Fatal(err)
	}
	receipts, err := store.SessionRecoveries(ctx, session.ID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("recovery receipt=%#v err=%v", receipts, err)
	}
	if safe, err := store.RecoveryNativeStateSafe(ctx, receipts[0]); err != nil || safe {
		t.Fatalf("ambiguous submission safety=%v err=%v", safe, err)
	}
	driver := &recoveryDriver{deleted: map[string]bool{}}
	service := persistence.RecoveryService{
		Store: store, Driver: driver, Queue: client.QueueName(), Claim: func(context.Context) error { return nil },
	}
	if _, err := service.Reconcile(ctx, session.ID); err == nil {
		t.Fatal("unsafe recovery progressed")
	}
	if driver.restoredSnapshot != "" || len(driver.deleted) != 0 {
		t.Fatal("unsafe recovery changed provider resources")
	}
	if held, err := store.SandboxDeliveryHeld(ctx, sandboxID); err != nil || !held {
		t.Fatalf("unsafe recovery released delivery: held=%v err=%v", held, err)
	}
	attention, err := store.Session(ctx, session.ID)
	if err != nil || attention.ExecutionAttentionSource != "recovery:"+request.ID || attention.ExecutionAttention == "" {
		t.Fatalf("unsafe recovery omitted attention: session=%#v err=%v", attention, err)
	}
}

// Distinct recoveries and upgrades race for one owner. An existing manual hold
// also blocks recovery. Losing requests must not leave a resource or hold.
func TestCheckpointRecoveryExcludesConcurrentMaintenance(t *testing.T) {
	for _, competing := range []string{"recovery", "upgrade", "hold"} {
		t.Run(competing, func(t *testing.T) {
			_, store, client := testDatabase(t)
			ctx := context.Background()
			session, sandboxID, boundary := completedCheckpointFixture(t, store, ctx, "maintenance-race")
			reference := persistence.Reference{Repository: "race-repository", SnapshotID: checkpointTestID(session.ID)}
			if _, err := store.PublishCheckpoint(ctx, boundary, reference); err != nil {
				t.Fatal(err)
			}
			request := persistence.RecoveryRequest{ID: "recover-" + session.ID, SessionID: session.ID, SandboxID: sandboxID,
				Repository: reference.Repository, SnapshotID: reference.SnapshotID}
			recover := func() error {
				_, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), request)
				return err
			}
			other := func() error {
				id := "competing-" + session.ID
				switch competing {
				case "recovery":
					r := request
					r.ID = id
					_, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), r)
					return err
				case "upgrade":
					_, err := store.RequestSandboxUpgrade(ctx, client.QueueName(), upgrade.Request{
						ID: id, SessionID: session.ID, SandboxID: sandboxID,
						PackagePath: "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-runner-0.155.0", Version: "0.155.0",
					})
					return err
				default:
					_, err := store.HoldSandboxDelivery(ctx, client.QueueName(), session.ID, sandboxID, id)
					return err
				}
			}
			var first, second error
			if competing == "hold" {
				// Manual holds can coexist; recovery must respect a pre-existing one.
				first, second = other(), recover()
			} else {
				start := make(chan struct{})
				results := make(chan error, 2)
				for _, request := range []func() error{recover, other} {
					go func() { <-start; results <- request() }()
				}
				close(start)
				first, second = <-results, <-results
			}
			if (first == nil) == (second == nil) {
				t.Fatalf("expected exactly one maintenance owner: %v / %v", first, second)
			}
			holds, err := store.SessionDeliveryHolds(ctx, session.ID)
			if err != nil || len(holds) != 1 || !holds[0].ReleasedAt.IsZero() {
				t.Fatalf("maintenance holds=%#v err=%v", holds, err)
			}
			receipts, err := store.SessionRecoveries(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			resources, err := store.SandboxResources(ctx, session.ID)
			if err != nil || len(resources) != 1+len(receipts) {
				t.Fatalf("losing request leaked a resource: resources=%d recoveries=%d err=%v", len(resources), len(receipts), err)
			}
		})
	}
}

func TestCheckpointRecoveryCleanupRemovesAbandonedDestination(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	session, sandboxID, boundary := completedCheckpointFixture(t, store, ctx, "recovery-cleanup")
	reference := persistence.Reference{Repository: "cleanup-repository", SnapshotID: checkpointTestID(session.ID + ":cleanup")}
	if _, err := store.PublishCheckpoint(ctx, boundary, reference); err != nil {
		t.Fatal(err)
	}
	request := persistence.RecoveryRequest{
		ID: "recover-cleanup-" + fmt.Sprint(time.Now().UnixNano()), SessionID: session.ID, SandboxID: sandboxID,
		Repository: reference.Repository, SnapshotID: reference.SnapshotID,
	}
	if _, err := store.RequestCheckpointRecovery(ctx, client.QueueName(), request); err != nil {
		t.Fatal(err)
	}
	driver := &recoveryDriver{deleted: map[string]bool{}}
	service := persistence.RecoveryService{
		Store: store, Driver: driver, Queue: client.QueueName(), Claim: func(context.Context) error { return nil },
	}
	// Cleanup owns the Session after a possible lost provider-create response.
	receipts, err := store.SessionRecoveries(ctx, session.ID)
	if err != nil || len(receipts) != 1 || receipts[0].DestinationResourceID == "" || receipts[0].DestinationProviderID != "" {
		t.Fatalf("reserved recovery receipt=%#v err=%v", receipts, err)
	}
	destinationID := receipts[0].DestinationResourceID
	if _, err := db.ExecContext(ctx, `update dorf.sessions set admission_open=false,cleanup_state='scheduled' where id=$1`, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareCleanup(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if !driver.deleted[destinationID] {
		t.Fatal("cleanup did not delete the abandoned replacement")
	}
	if held, err := store.SandboxDeliveryHeld(ctx, sandboxID); err != nil || held {
		t.Fatalf("cleanup retained abandoned recovery hold: held=%v err=%v", held, err)
	}
	cleanupBoundary, err := store.Boundary(ctx, sandboxID, true)
	if err != nil || !cleanupBoundary.Eligible {
		t.Fatalf("ordinary cleanup checkpoint remained ineligible: boundary=%#v err=%v", cleanupBoundary, err)
	}
	if progressed, err := service.Reconcile(ctx, session.ID); err != nil || progressed {
		t.Fatalf("cleanup resumed abandoned recovery: progressed=%v err=%v", progressed, err)
	}
	if driver.restoredSnapshot != "" {
		t.Fatalf("cleanup restored abandoned recovery snapshot %q", driver.restoredSnapshot)
	}
	if err := service.PrepareCleanup(ctx, session.ID); err != nil {
		t.Fatalf("cleanup recovery reconciliation was not idempotent: %v", err)
	}
	resources, err := store.SandboxResources(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		if resource.ID == destinationID && resource.DeletedAt.IsZero() {
			t.Fatal("cleanup did not retain replacement deletion fact")
		}
	}
	active, err := store.Sandbox(ctx, sandboxID)
	if err != nil || active.ResourceID != boundary.ResourceID {
		t.Fatalf("cleanup adopted abandoned replacement: sandbox=%#v err=%v", active, err)
	}
}

func checkpointTestID(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}
