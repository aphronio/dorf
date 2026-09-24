package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestCheckpointPublicationIsImmutableIdempotentAndBoundaryChecked(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	_, sandboxID, boundary := completedCheckpointFixture(t, store, ctx, "publication")
	firstRef := checkpointReference("test-repository", boundary.SessionID+":first")

	first, err := store.PublishCheckpoint(ctx, boundary, firstRef)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.PublishCheckpoint(ctx, boundary, firstRef)
	if err != nil || replayed != first {
		t.Fatalf("lost acknowledgement replay changed checkpoint: checkpoint=%#v err=%v", replayed, err)
	}

	if _, err := db.ExecContext(ctx, `update dorf.sessions set sandbox_last_active_at=sandbox_last_active_at-interval '1 second' where id=$1`, boundary.SessionID); err != nil {
		t.Fatal(err)
	}
	current, err := store.Boundary(ctx, sandboxID, false)
	if err != nil || !current.Eligible || current == boundary {
		t.Fatalf("activity did not advance the boundary: current=%#v err=%v", current, err)
	}
	// A replay recognizes the already-published fact without making it current
	// again. A new reference for the obsolete upload cannot enter history.
	replayed, err = store.PublishCheckpoint(ctx, boundary, firstRef)
	if err != nil || replayed != first {
		t.Fatalf("stale acknowledgement replay was not idempotent: checkpoint=%#v err=%v", replayed, err)
	}
	if _, err := store.PublishCheckpoint(ctx, boundary, checkpointReference("test-repository", boundary.SessionID+":obsolete")); !errors.Is(err, persistence.ErrCheckpointSuperseded) {
		t.Fatalf("obsolete upload publication error=%v", err)
	}
	last, err := store.LastCheckpoint(ctx, sandboxID)
	if err != nil || last != first {
		t.Fatalf("obsolete attempt moved authoritative checkpoint: last=%#v err=%v", last, err)
	}

	second, err := store.PublishCheckpoint(ctx, current, checkpointReference("test-repository", boundary.SessionID+":second"))
	if err != nil {
		t.Fatal(err)
	}
	last, err = store.LastCheckpoint(ctx, sandboxID)
	if err != nil || last != second {
		t.Fatalf("latest successful boundary was not authoritative: last=%#v err=%v", last, err)
	}
	history, err := store.ListCheckpoints(ctx, sandboxID)
	if err != nil || len(history) != 2 || history[0] != second || history[1] != first {
		t.Fatalf("immutable checkpoint history=%#v err=%v", history, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.sandbox_checkpoints set native_revision=native_revision+1 where repository=$1 and snapshot_id=$2`, firstRef.Repository, firstRef.SnapshotID); err == nil {
		t.Fatal("published checkpoint accepted in-place mutation")
	}
}

func TestCheckpointPublicationRejectsSettledConcurrentChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, context.Context, postgres.Store, string, persistence.CaptureBoundary)
	}{
		{
			name: "activity",
			mutate: func(t *testing.T, ctx context.Context, store postgres.Store, _ string, boundary persistence.CaptureBoundary) {
				t.Helper()
				if _, err := store.DB.ExecContext(ctx, `update dorf.sessions set sandbox_last_active_at=sandbox_last_active_at-interval '1 second' where id=$1`, boundary.SessionID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "input",
			mutate: func(t *testing.T, ctx context.Context, store postgres.Store, sandboxID string, boundary persistence.CaptureBoundary) {
				t.Helper()
				advanceNativeFixture(t, store, ctx, boundary.SessionID, "thread-retained", "concurrent-input")
			},
		},
		{
			name: "resource-replacement",
			mutate: func(t *testing.T, ctx context.Context, store postgres.Store, sandboxID string, boundary persistence.CaptureBoundary) {
				t.Helper()
				replacementID := sandboxID + ":replacement"
				if _, err := store.DB.ExecContext(ctx, `insert into dorf.sandbox_resources(id,sandbox_id,ownership_nonce,provider_id,observed_at)
values($1,$2,$3,'provider-replacement',clock_timestamp())`, replacementID, sandboxID, fmt.Sprintf("%x", sha256.Sum256([]byte(boundary.SessionID+":replacement")))); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB.ExecContext(ctx, `update dorf.sandboxes set active_resource_id=$1 where id=$2`, replacementID, sandboxID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "completed-hold",
			mutate: func(t *testing.T, ctx context.Context, store postgres.Store, sandboxID string, _ persistence.CaptureBoundary) {
				t.Helper()
				if _, err := store.DB.ExecContext(ctx, `insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason,released_at)
values($1,$2,'workspace_upgrade',clock_timestamp())`, "hold-"+sandboxID, sandboxID); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			_, sandboxID, expected := completedCheckpointFixture(t, store, ctx, "race-"+tc.name)
			tc.mutate(t, ctx, store, sandboxID, expected)
			current, err := store.Boundary(ctx, sandboxID, false)
			if err != nil || !current.Eligible || current == expected {
				t.Fatalf("settled mutation did not produce a new eligible boundary: current=%#v err=%v", current, err)
			}
			_, err = store.PublishCheckpoint(ctx, expected, checkpointReference("race-repository", expected.SessionID))
			if !errors.Is(err, persistence.ErrCheckpointSuperseded) {
				t.Fatalf("late publication error=%v", err)
			}
			if _, err := store.LastCheckpoint(ctx, sandboxID); !errors.Is(err, persistence.ErrCheckpointNotFound) {
				t.Fatalf("late publication entered successful history: %v", err)
			}
		})
	}
}

func TestCheckpointRetainsEffectivePackageGenerationAndCleanupUsesSameHistory(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	_, sandboxID, initial := completedCheckpointFixture(t, store, ctx, "generation-cleanup")
	upgradeID := "upgrade-" + fmt.Sprint(time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason,released_at)
values($1,$2,'workspace_upgrade',clock_timestamp())`, upgradeID, sandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_upgrades(
    id,sandbox_id,source_resource_id,package_path,version,previous_version,
    quiesced_at,checkpoint_key,checkpoint_reference,checkpoint_source_id,
    activated_at,verified_at,checkpoint_deleted_at,finished_at
) values(
    $1,$2,$3,$4,'1.2.3','1.2.2',clock_timestamp(),$1,'provider-checkpoint',$5,
    clock_timestamp(),clock_timestamp(),clock_timestamp(),clock_timestamp()
)`, upgradeID, sandboxID, initial.ResourceID, "/nix/store/"+strings.Repeat("f", 32)+"-runner-1.2.3", "provider-original"); err != nil {
		t.Fatal(err)
	}
	upgraded, err := store.Boundary(ctx, sandboxID, false)
	if err != nil || upgraded.EffectiveUpgradeID != upgradeID || upgraded.DeliveryHoldCount != 1 || !upgraded.Eligible {
		t.Fatalf("effective package generation boundary=%#v err=%v", upgraded, err)
	}
	checkpoint, err := store.PublishCheckpoint(ctx, upgraded, checkpointReference("generation-repository", upgraded.SessionID+":upgrade"))
	if err != nil || checkpoint.EffectiveUpgradeID != upgradeID {
		t.Fatalf("published package generation=%#v err=%v", checkpoint, err)
	}

	if _, err := db.ExecContext(ctx, `update dorf.sessions set admission_open=false,cleanup_state='requested' where id=$1`, upgraded.SessionID); err != nil {
		t.Fatal(err)
	}
	normal, err := store.Boundary(ctx, sandboxID, false)
	if err != nil || normal.Eligible {
		t.Fatalf("closed admission remained normal-capture eligible: boundary=%#v err=%v", normal, err)
	}
	cleanup, err := store.Boundary(ctx, sandboxID, true)
	if err != nil || !cleanup.Eligible {
		t.Fatalf("settled cleanup boundary was not eligible: boundary=%#v err=%v", cleanup, err)
	}
	final, err := store.PublishCheckpoint(ctx, cleanup, checkpointReference("generation-repository", upgraded.SessionID+":cleanup"))
	if err != nil || !final.Cleanup || final.EffectiveUpgradeID != upgradeID {
		t.Fatalf("cleanup did not use ordinary checkpoint history: checkpoint=%#v err=%v", final, err)
	}
	history, err := store.ListCheckpoints(ctx, sandboxID)
	if err != nil || len(history) != 2 || !history[0].Cleanup || history[1].Cleanup {
		t.Fatalf("shared checkpoint history=%#v err=%v", history, err)
	}
}

func TestEmptyNativeCleanupCanCheckpointAfterLostActivityReceipt(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	session, created, err := store.AdmitDirect(ctx, core.SessionAdmission{
		AdmissionKey:   "empty-native-cleanup-" + fmt.Sprint(time.Now().UnixNano()),
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low",
	}, client.QueueName())
	if err != nil || !created {
		t.Fatalf("admit empty native Session: created=%t err=%v", created, err)
	}
	sandboxID := core.MainSandboxName(session.ID)
	owned, err := store.Sandbox(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSandboxResource(ctx, owned, "provider-empty-native"); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginSandboxActivity(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.sessions set admission_open=false,cleanup_state='requested' where id=$1`, session.ID); err != nil {
		t.Fatal(err)
	}

	ordinary, err := store.Boundary(ctx, sandboxID, false)
	if err != nil || ordinary.Eligible || ordinary.LastActivityAt.IsZero() {
		t.Fatalf("ordinary boundary repaired lost activity receipt: boundary=%#v err=%v", ordinary, err)
	}
	cleanup, err := store.Boundary(ctx, sandboxID, true)
	if err != nil || !cleanup.Eligible || cleanup.LastActivityAt.IsZero() || cleanup.NativeRevision != 0 {
		t.Fatalf("empty native cleanup boundary=%#v err=%v", cleanup, err)
	}
	replayed, err := store.Boundary(ctx, sandboxID, true)
	if err != nil || replayed != cleanup {
		t.Fatalf("cleanup fallback boundary changed: first=%#v replay=%#v err=%v", cleanup, replayed, err)
	}
	checkpoint, err := store.PublishCheckpoint(ctx, cleanup, checkpointReference("empty-native-cleanup-repository", session.ID))
	if err != nil || !checkpoint.Cleanup || checkpoint.NativeRevision != 0 {
		t.Fatalf("empty native cleanup checkpoint=%#v err=%v", checkpoint, err)
	}
}

func TestCheckpointCandidateScanReturnsOnlyChangedIdleBoundaries(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	_, sandboxID, boundary := completedCheckpointFixture(t, store, ctx, "candidate")
	// The shared test database may contain more than one scan page of fixtures.
	// Put this fixture first and retire it when the proof ends.
	if _, err := store.DB.ExecContext(ctx, `update dorf.sessions set sandbox_last_active_at='2000-01-01' where id=$1`, boundary.SessionID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.DB.ExecContext(context.Background(), `update dorf.sessions set admission_open=false where id=$1`, boundary.SessionID); err != nil {
			t.Error(err)
		}
	})
	var err error
	boundary, err = store.Boundary(ctx, sandboxID, false)
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointCandidate(t, store, ctx, sandboxID, boundary, true)

	if _, err := store.PublishCheckpoint(ctx, boundary, checkpointReference("candidate-repository", boundary.SessionID)); err != nil {
		t.Fatal(err)
	}
	assertCheckpointCandidate(t, store, ctx, sandboxID, boundary, false)

	if _, err := store.DB.ExecContext(ctx, `update dorf.sessions set sandbox_last_active_at=sandbox_last_active_at-interval '1 second' where id=$1`, boundary.SessionID); err != nil {
		t.Fatal(err)
	}
	changed, err := store.Boundary(ctx, sandboxID, false)
	if err != nil {
		t.Fatal(err)
	}
	assertCheckpointCandidate(t, store, ctx, sandboxID, changed, true)
}

func completedCheckpointFixture(t *testing.T, store postgres.Store, ctx context.Context, suffix string) (core.Session, string, persistence.CaptureBoundary) {
	t.Helper()
	session, _, err := admitDirectFixture(t, store, ctx, core.SessionAdmission{
		AdmissionKey:   "checkpoint-" + suffix + "-" + fmt.Sprint(time.Now().UnixNano()),
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := core.MainSandboxName(session.ID)
	owned, err := store.Sandbox(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindSandboxResource(ctx, owned, "provider-original"); err != nil {
		t.Fatal(err)
	}
	advanceNativeFixture(t, store, ctx, session.ID, "thread-retained", "initial-input")
	session.ThreadID = "thread-retained"
	if err := store.FinishSandboxActivity(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	boundary, err := store.Boundary(ctx, sandboxID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !boundary.Eligible || boundary.NativeRevision != 1 || boundary.DeliveryHoldCount != 0 {
		t.Fatalf("completed fixture boundary=%#v", boundary)
	}
	return session, sandboxID, boundary
}

func checkpointReference(repository, seed string) persistence.Reference {
	return persistence.Reference{Repository: repository, SnapshotID: fmt.Sprintf("%x", sha256.Sum256([]byte(seed)))}
}

func assertCheckpointCandidate(t *testing.T, store postgres.Store, ctx context.Context, sandboxID string, expected persistence.CaptureBoundary, want bool) {
	t.Helper()
	candidates, err := store.ListCheckpointCandidates(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		if candidate.SandboxID == sandboxID {
			found = candidate == expected
			break
		}
	}
	if found != want {
		t.Fatalf("candidate %s found=%t want=%t candidates=%#v", sandboxID, found, want, candidates)
	}
}

func advanceNativeFixture(t *testing.T, store postgres.Store, ctx context.Context, sessionID, threadID, inputID string) {
	t.Helper()
	if err := store.BindNativeThread(ctx, sessionID, threadID); err != nil {
		t.Fatal(err)
	}
	revision, err := store.BeginNativeMutation(ctx, sessionID, threadID, inputID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishNativeMutation(ctx, sessionID, revision); err != nil {
		t.Fatal(err)
	}
}

func TestCopiedCheckpointPublishesHistoricalBoundaryAndSurvivesRestart(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := t.Context()
	_, sandboxID, before := completedCheckpointFixture(t, store, ctx, "background-copy")
	if _, err := db.ExecContext(ctx, `update dorf.sessions set native_revision=native_revision+1 where id=$1`, before.SessionID); err != nil {
		t.Fatal(err)
	}
	current, err := store.Boundary(ctx, sandboxID, false)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.PublishCheckpoint(ctx, current, checkpointReference("copy-repository", before.SessionID+":new"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.PublishCapturedCheckpoint(ctx, before, checkpointReference("copy-repository", before.SessionID+":old"), "cp_"+before.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	reopened := postgres.Store{DB: db}
	observed, err := reopened.CheckpointByID(ctx, saved.ID)
	if err != nil || observed != saved {
		t.Fatal("published ID lost across store recreation")
	}
	last, err := store.LastCheckpoint(ctx, sandboxID)
	if err != nil || last != newer {
		t.Fatal("late upload replaced newer recovery point")
	}
}
