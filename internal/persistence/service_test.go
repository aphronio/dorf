package persistence

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type captureStoreFixture struct {
	mu            sync.Mutex
	boundary      CaptureBoundary
	published     int
	beforePublish func()
}

func (s *captureStoreFixture) Boundary(context.Context, string, bool) (CaptureBoundary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boundary, nil
}
func (s *captureStoreFixture) LastCheckpoint(context.Context, string) (Checkpoint, error) {
	return Checkpoint{}, nil
}
func (s *captureStoreFixture) PublishCheckpoint(_ context.Context, b CaptureBoundary, r Reference) (Checkpoint, error) {
	if s.beforePublish != nil {
		s.beforePublish()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.boundary != b {
		return Checkpoint{}, ErrCheckpointSuperseded
	}
	s.published++
	return Checkpoint{}, nil
}
func (s *captureStoreFixture) admit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boundary.MessageSequence++
	s.boundary.Eligible = false
}

type captureFunc func(context.Context, CaptureBoundary) (Reference, error)

func (f captureFunc) Capture(ctx context.Context, b CaptureBoundary) (Reference, error) {
	return f(ctx, b)
}
func captureServiceFixture(driver Capturer) (Service, *captureStoreFixture) {
	store := &captureStoreFixture{boundary: CaptureBoundary{JobID: "job-test", SandboxID: "sandbox-test", ResourceID: "resource-test", Eligible: true, LastActivityAt: time.Now().Add(-time.Minute), CompletedTurnSequence: 1}}
	return Service{Store: store, Driver: driver, Claim: func(context.Context) error { return nil }}, store
}

func TestInputInvalidatesBackupWithoutWaitingForRemoteStop(t *testing.T) {
	started, stopping, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	driver := captureFunc(func(ctx context.Context, _ CaptureBoundary) (Reference, error) {
		close(started)
		<-ctx.Done()
		close(stopping)
		<-release
		// A late success is deliberately returned after cancellation.
		return Reference{Repository: "test", SnapshotID: "late-success"}, nil
	})
	service, store := captureServiceFixture(driver)
	claims := 0
	service.Claim = func(context.Context) error { claims++; return nil }
	done := make(chan error, 1)
	go func() { _, err := service.Capture(context.Background(), "sandbox-test", false); done <- err }()
	<-started
	store.admit()
	select {
	case <-stopping:
	case <-time.After(time.Second):
		t.Fatal("backup did not request cancellation")
	}
	// Admission finished while actual remote cancellation is still in progress.
	select {
	case <-done:
		t.Fatal("capture returned before remote stop observation")
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrCheckpointSuperseded) {
		t.Fatalf("late result = %v", err)
	}
	if store.published != 0 {
		t.Fatal("cancelled backup became authoritative")
	}
	if claims != 1 {
		t.Fatalf("cancelled backup claim checks = %d, want no publication claim", claims)
	}
}

func TestInputAtPublicationRejectsAlreadyUploadedSnapshot(t *testing.T) {
	service, store := captureServiceFixture(captureFunc(func(context.Context, CaptureBoundary) (Reference, error) {
		return Reference{Repository: "test", SnapshotID: "uploaded"}, nil
	}))
	store.beforePublish = store.admit
	_, err := service.Capture(context.Background(), "sandbox-test", false)
	if !errors.Is(err, ErrCheckpointSuperseded) || store.published != 0 {
		t.Fatalf("publication race = %v, published %d", err, store.published)
	}
}

func TestIdleDebounceAndCleanupUseSameCapture(t *testing.T) {
	calls := 0
	service, store := captureServiceFixture(captureFunc(func(context.Context, CaptureBoundary) (Reference, error) {
		calls++
		return Reference{Repository: "test", SnapshotID: "complete"}, nil
	}))
	store.boundary.LastActivityAt = time.Now()
	if _, err := service.Capture(context.Background(), "sandbox-test", false); !errors.Is(err, ErrCheckpointIneligible) {
		t.Fatalf("debounce = %v", err)
	}
	if calls != 0 {
		t.Fatal("backup started inside debounce")
	}
	if _, err := service.Capture(context.Background(), "sandbox-test", true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || store.published != 1 {
		t.Fatal("cleanup did not publish through ordinary capture")
	}
}

func TestCancelledTaskDoesNotMasqueradeAsBoundarySupersession(t *testing.T) {
	started := make(chan struct{})
	service, store := captureServiceFixture(captureFunc(func(ctx context.Context, _ CaptureBoundary) (Reference, error) {
		close(started)
		<-ctx.Done()
		return Reference{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := service.Capture(ctx, "sandbox-test", false); done <- err }()
	<-started
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture error = %v", err)
	}
	if errors.Is(err, ErrCheckpointSuperseded) {
		t.Fatalf("task cancellation was reported as boundary supersession: %v", err)
	}
	if store.published != 0 {
		t.Fatal("cancelled task published a checkpoint")
	}
}

func TestLostTaskClaimCannotPublishCompletedUpload(t *testing.T) {
	service, store := captureServiceFixture(captureFunc(func(context.Context, CaptureBoundary) (Reference, error) {
		return Reference{Repository: "test", SnapshotID: "uploaded"}, nil
	}))
	claimLost := errors.New("task claim lost")
	claims := 0
	service.Claim = func(context.Context) error {
		claims++
		if claims == 2 {
			return claimLost
		}
		return nil
	}
	_, err := service.Capture(context.Background(), "sandbox-test", false)
	if !errors.Is(err, claimLost) {
		t.Fatalf("lost claim error = %v", err)
	}
	if claims != 2 {
		t.Fatalf("claim checks = %d, want 2", claims)
	}
	if store.published != 0 {
		t.Fatal("upload published after its task claim was lost")
	}
}
