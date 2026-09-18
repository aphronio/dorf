package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type workerStoreFixture struct {
	mu         sync.Mutex
	boundaries map[string]CaptureBoundary
	candidates []CaptureBoundary
	published  []Checkpoint
}

func (s *workerStoreFixture) Boundary(_ context.Context, sandboxID string, cleanup bool) (CaptureBoundary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	boundary, ok := s.boundaries[sandboxID]
	if !ok {
		return CaptureBoundary{}, ErrCheckpointNotFound
	}
	boundary.Cleanup = cleanup
	return boundary, nil
}

func (s *workerStoreFixture) LastCheckpoint(_ context.Context, sandboxID string) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.published) - 1; i >= 0; i-- {
		if s.published[i].SandboxID == sandboxID {
			return s.published[i], nil
		}
	}
	return Checkpoint{}, ErrCheckpointNotFound
}

func (s *workerStoreFixture) PublishCheckpoint(_ context.Context, expected CaptureBoundary, reference Reference) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.boundaries[expected.SandboxID]; !ok || current != expected {
		return Checkpoint{}, ErrCheckpointSuperseded
	}
	checkpoint := Checkpoint{CaptureBoundary: expected, Reference: reference, PublishedAt: time.Now().UTC()}
	s.published = append(s.published, checkpoint)
	return checkpoint, nil
}

func (s *workerStoreFixture) ListCheckpointCandidates(context.Context, time.Duration) ([]CaptureBoundary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]CaptureBoundary(nil), s.candidates...), nil
}

func TestBackupQueueDoesNotConsumeOneSlotForegroundCapacity(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var schemaVersion string
	if err := db.QueryRowContext(t.Context(), `select absurd.get_schema_version()`).Scan(&schemaVersion); err != nil {
		t.Fatalf("Absurd schema is not ready: %v", err)
	}
	if schemaVersion != "0.5.0" {
		t.Fatalf("Absurd schema version = %q", schemaVersion)
	}

	suffix := time.Now().UnixNano()
	foreground := persistenceTestQueue(t, db, fmt.Sprintf("checkpoint_foreground_%d", suffix))
	backup := persistenceTestQueue(t, db, fmt.Sprintf("checkpoint_backup_%d", suffix))
	boundary := CaptureBoundary{
		SessionID: "job-backup-capacity", SandboxID: "sandbox-backup-capacity", ResourceID: "resource-backup-capacity",
		ProfileName: "codex-e2b", ProfileRevision: fmt.Sprintf("%064x", suffix),
		LastActivityAt: time.Now().Add(-time.Minute), CompletedTurnSequence: 1, Eligible: true,
	}
	wrongRevision := boundary
	wrongRevision.SandboxID = "sandbox-wrong-revision"
	wrongRevision.ResourceID = "resource-wrong-revision"
	wrongRevision.ProfileRevision = fmt.Sprintf("%064x", suffix+1)
	store := &workerStoreFixture{
		boundaries: map[string]CaptureBoundary{boundary.SandboxID: boundary, wrongRevision.SandboxID: wrongRevision},
		candidates: []CaptureBoundary{wrongRevision, boundary},
	}

	backupStarted := make(chan struct{})
	releaseBackup := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBackup) }) }
	t.Cleanup(release)
	resolved := 0
	worker := Worker{
		Store: store, Tasks: backup, IdleDelay: time.Second,
		Enabled: func(candidate CaptureBoundary) bool {
			return candidate.ProfileName == boundary.ProfileName && candidate.ProfileRevision == boundary.ProfileRevision
		},
		Resolve: func(_ context.Context, candidate CaptureBoundary) (Service, error) {
			resolved++
			if candidate.SandboxID != boundary.SandboxID {
				return Service{}, fmt.Errorf("resolved disabled profile candidate %s", candidate.SandboxID)
			}
			return Service{Store: store, Claim: func(context.Context) error { return nil }, Driver: captureFunc(func(context.Context, CaptureBoundary) (Reference, error) {
				close(backupStarted)
				<-releaseBackup
				return Reference{Repository: "test", SnapshotID: fmt.Sprintf("%064x", suffix)}, nil
			})}, nil
		},
	}
	worker.Register(t.Context())
	if err := worker.discover(t.Context()); err != nil {
		t.Fatal(err)
	}
	backupDone := make(chan error, 1)
	go func() {
		backupDone <- backup.WorkBatch(context.Background(), absurd.WorkBatchOptions{WorkerID: "checkpoint-backup", BatchSize: 1, ClaimTimeout: time.Minute})
	}()
	select {
	case <-backupStarted:
	case <-time.After(time.Second):
		t.Fatal("backup task did not start")
	}

	foregroundRan := make(chan struct{})
	foreground.MustRegister(absurd.Task("checkpoint-foreground-proof-v1", func(context.Context, struct{}) (string, error) {
		close(foregroundRan)
		return "completed", nil
	}))
	if _, err := foreground.Spawn(t.Context(), "checkpoint-foreground-proof-v1", struct{}{}, absurd.SpawnOptions{}); err != nil {
		t.Fatal(err)
	}
	foregroundDone := make(chan error, 1)
	go func() {
		foregroundDone <- foreground.WorkBatch(context.Background(), absurd.WorkBatchOptions{WorkerID: "foreground-one-slot", BatchSize: 1, ClaimTimeout: time.Minute})
	}()
	select {
	case err := <-foregroundDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("one-slot foreground queue waited for the blocked backup queue")
	}
	select {
	case <-foregroundRan:
	default:
		t.Fatal("foreground task did not run")
	}
	select {
	case err := <-backupDone:
		t.Fatalf("backup unexpectedly stopped before release: %v", err)
	default:
	}

	release()
	if err := <-backupDone; err != nil {
		t.Fatal(err)
	}
	if resolved != 1 {
		t.Fatalf("resolved %d candidates; want only the exact configured profile", resolved)
	}
	if len(store.published) != 1 || store.published[0].SandboxID != boundary.SandboxID {
		t.Fatalf("published checkpoints = %#v", store.published)
	}
}

func persistenceTestQueue(t *testing.T, db *sql.DB, name string) *absurd.Client {
	t.Helper()
	client, err := absurd.New(absurd.Options{DB: db, QueueName: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CreateQueue(t.Context(), name); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.DropQueue(context.Background(), name); err != nil {
			t.Errorf("drop queue %q: %v", name, err)
		}
		if err := client.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
			t.Errorf("close queue %q: %v", name, err)
		}
	})
	return client
}
