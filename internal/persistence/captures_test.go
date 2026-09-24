package persistence

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRemoteCaptureRequiresConfirmationAndRejectsLostGuard(t *testing.T) {
	for _, action := range []string{"commit", "source-change", "cancel", "restart"} {
		t.Run(action, func(t *testing.T) {
			service, store := captureServiceFixture(guardedCaptureFunc(func(ctx context.Context, _ CaptureBoundary, pin func(context.Context, Reference) error) (Reference, error) {
				reference := Reference{Repository: "synthetic", SnapshotID: "immutable"}
				return reference, pin(ctx, reference)
			}))
			captures := &Captures{}
			defer captures.Close()
			initial, err := captures.Start("source", "sandbox", service)
			if err != nil {
				t.Fatal(err)
			}
			ready := awaitCapture(t, captures, initial.ID, "ready")
			if ready.Reference == nil || ready.Checkpoint != nil {
				t.Fatal("provisional reference confused with publication")
			}
			if _, err := captures.Start("source", "sandbox", service); !errors.Is(err, ErrCaptureConflict) {
				t.Fatalf("concurrent guard: %v", err)
			}
			switch action {
			case "commit":
				if _, err := captures.Observe(initial.ID, "commit"); err != nil {
					t.Fatal(err)
				}
				awaitCapture(t, captures, initial.ID, "published")
				replay, err := captures.Observe(initial.ID, "commit")
				if err != nil || replay.Checkpoint == nil {
					t.Fatal("lost commit response cannot be reconciled")
				}
			case "source-change":
				store.admit()
				awaitCapture(t, captures, initial.ID, "failed")
				if _, err := captures.Observe(initial.ID, "commit"); !errors.Is(err, ErrCaptureConflict) {
					t.Fatal("stale guard accepted commit")
				}
			case "cancel":
				if _, err := captures.Observe(initial.ID, "cancel"); err != nil {
					t.Fatal(err)
				}
				awaitCapture(t, captures, initial.ID, "cancelled")
			case "restart":
				captures.Close()
				awaitCapture(t, captures, initial.ID, "cancelled")
				restarted := &Captures{}
				if _, err := restarted.Observe(initial.ID, "commit"); !errors.Is(err, ErrCaptureNotFound) {
					t.Fatal("replacement worker accepted lost guard")
				}
			}
			store.mu.Lock()
			published := store.published
			store.mu.Unlock()
			if (published == 1) != (action == "commit") {
				t.Fatalf("published=%d after %s", published, action)
			}
		})
	}
}

func awaitCapture(t *testing.T, captures *Captures, id, state string) CaptureAttempt {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, err := captures.Observe(id, "status")
		if err != nil {
			t.Fatal(err)
		}
		if result.State == state {
			return result
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("capture did not reach %s", state)
	return CaptureAttempt{}
}
