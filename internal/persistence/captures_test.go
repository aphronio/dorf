package persistence

import (
	"context"
	"errors"
	"testing"
	"time"
)

type copyCaptureFunc func(context.Context, CaptureBoundary, func(context.Context) error) (Reference, error)

func (f copyCaptureFunc) Capture(ctx context.Context, b CaptureBoundary) (Reference, error) {
	return f(ctx, b, func(context.Context) error { return nil })
}
func (f copyCaptureFunc) CaptureCopy(ctx context.Context, b CaptureBoundary, copied func(context.Context) error) (Reference, error) {
	return f(ctx, b, copied)
}
func (s *captureStoreFixture) PublishCapturedCheckpoint(_ context.Context, b CaptureBoundary, r Reference, id string) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published++
	return Checkpoint{ID: id, CaptureBoundary: b, Reference: r}, nil
}

func TestCheckpointCopySeparatesLiveStateFromUpload(t *testing.T) {
	for _, failure := range []string{"", "copy", "upload", "restart"} {
		t.Run(failure, func(t *testing.T) {
			upload := make(chan struct{})
			defer close(upload)
			var store *captureStoreFixture
			driver := copyCaptureFunc(func(ctx context.Context, b CaptureBoundary, copied func(context.Context) error) (Reference, error) {
				if failure == "copy" {
					store.admit()
				}
				if err := copied(ctx); err != nil {
					return Reference{}, err
				}
				select {
				case <-upload:
				case <-ctx.Done():
					return Reference{}, ctx.Err()
				}
				if failure == "upload" {
					return Reference{}, errors.New("upload failed")
				}
				return Reference{Repository: "synthetic", SnapshotID: "immutable"}, nil
			})
			service, fixture := captureServiceFixture(driver)
			store = fixture
			captures := &Captures{}
			defer captures.Close()
			initial, err := captures.Start("source", "sandbox", service, "same-request")
			if err != nil {
				t.Fatal(err)
			}
			replay, err := captures.Start("source", "sandbox", service, "same-request")
			if err != nil || replay.ID != initial.ID {
				t.Fatal("lost start response cannot be retried")
			}
			if failure == "copy" {
				awaitCapture(t, captures, initial.ID, "failed")
				return
			}
			copying := awaitCapture(t, captures, initial.ID, "uploading")
			if copying.Checkpoint != nil || copying.Boundary == nil {
				t.Fatal("copy mistaken for durable checkpoint")
			}
			store.admit() // New input during upload must not invalidate the copied tree.
			if failure == "restart" {
				captures.Close()
				awaitCapture(t, captures, initial.ID, "cancelled")
				return
			}
			upload <- struct{}{}
			state := "ready"
			if failure != "" {
				state = "failed"
			}
			final := awaitCapture(t, captures, initial.ID, state)
			if failure == "" && (final.Checkpoint == nil || final.Checkpoint.NativeRevision != 1) {
				t.Fatal("saved boundary lost after source advanced")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if (store.published == 1) != (failure == "") {
				t.Fatal("failed upload published")
			}
		})
	}
}
func awaitCapture(t *testing.T, c *Captures, id, state string) CaptureAttempt {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v, e := c.Observe(id, "status")
		if e != nil {
			t.Fatal(e)
		}
		if v.State == state {
			return v
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("checkpoint did not reach %s", state)
	return CaptureAttempt{}
}
