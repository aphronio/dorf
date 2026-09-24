package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
)

type idleIntegrationExternals struct {
	core.Externals
	pause func() error
}

func (e idleIntegrationExternals) SandboxPause(context.Context, core.Session, core.Sandbox) error {
	return e.pause()
}

func TestIdlePolicyAdmissionAndReplay(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	service := direct.NewAdmissionService(store, client.QueueName(), providerCheck{})
	for _, keep := range []bool{false, true} {
		request := direct.AdmissionRequest{KeepRunning: keep, AdmissionKey: fmt.Sprintf("idle-policy-%t-%d", keep, time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"}
		session, created, err := service.Admit(ctx, request)
		if err != nil || !created || session.KeepRunning != keep {
			t.Fatalf("admit=%+v %v", session, err)
		}
		// A fresh service reloads the same immutable override after restart.
		restarted := direct.NewAdmissionService(store, client.QueueName(), providerCheck{})
		replay, created, err := restarted.Admit(ctx, request)
		if err != nil || created || replay.KeepRunning != keep {
			t.Fatalf("replay=%+v %v", replay, err)
		}
		request.KeepRunning = !keep
		if _, _, err := restarted.Admit(ctx, request); !errors.Is(err, direct.ErrAdmissionConflict) {
			t.Fatalf("changed policy replay=%v", err)
		}
	}
}

// Admission can acknowledge while a provider call is in progress. Native
// delivery is fenced; the queued message must invalidate the next idle check.

func TestSandboxIdleGracePersistsAcrossRestartAndInterruptedActivity(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	session, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("idle-grace-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	external := idleIntegrationExternals{pause: func() error { calls++; return nil }}
	reconcile := func() {
		t.Helper()
		if err := core.NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(ctx, session.ID); err != nil {
			t.Fatal(err)
		}
	}
	activityTime := func() time.Time {
		t.Helper()
		var at time.Time
		if err := db.QueryRowContext(ctx, "select sandbox_last_active_at from dorf.sessions where id=$1", session.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	age := func(seconds int) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "update dorf.sessions set sandbox_last_active_at=clock_timestamp()-make_interval(secs=>$2) where id=$1", session.ID, seconds); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	age(59)
	reconcile()
	if calls != 0 {
		t.Fatal("paused before the minute elapsed")
	}
	age(61)
	before := activityTime()
	reconcile()
	reconcile()
	if calls != 2 || !activityTime().Equal(before) {
		t.Fatal("idle polling changed activity or missed eligible pause")
	}
	if err := store.WithSessionFence(ctx, session.ID, func() error { return store.BeginSandboxActivity(ctx, session.ID) }); err != nil {
		t.Fatal(err)
	}
	reconcile()
	recovered := activityTime()
	reconcile()
	if calls != 2 || !activityTime().Equal(recovered) {
		t.Fatal("recovery did not retain one fresh grace period")
	}
	age(61)
	failure := errors.New("file read failed after waking")
	if err := store.WithSessionFence(ctx, session.ID, func() error {
		return core.WithSandboxActivity(ctx, store, session.ID, func() error { return failure })
	}); !errors.Is(err, failure) {
		t.Fatalf("operation error=%v", err)
	}
	reconcile()
	if calls != 2 {
		t.Fatal("failed access lost its fresh grace period")
	}
}

func TestCanceledSandboxActivityKeepsFenceUntilFinishIsRecorded(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	session, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("idle-cancel-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "update dorf.sessions set sandbox_last_active_at=clock_timestamp()-interval '2 minutes' where id=$1", session.ID); err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- store.WithSessionFence(requestCtx, session.ID, func() error {
			return core.WithSandboxActivity(requestCtx, store, session.ID, func() error {
				close(entered)
				select {
				case <-release:
					return requestCtx.Err()
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	var calls atomic.Int32
	external := idleIntegrationExternals{pause: func() error { calls.Add(1); return nil }}
	paused := make(chan error, 1)
	go func() {
		paused <- core.NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(ctx, session.ID)
	}()
	select {
	case err := <-paused:
		t.Fatalf("cancellation released a still-running operation's fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("operation error=%v", err)
	}
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("paused immediately after canceled operation")
	}
	var recent bool
	if err := db.QueryRowContext(ctx, "select sandbox_last_active_at > clock_timestamp()-interval '5 seconds' from dorf.sessions where id=$1", session.ID).Scan(&recent); err != nil || !recent {
		t.Fatalf("finish was not durable: recent=%v error=%v", recent, err)
	}
}

func TestBackgroundSandboxUseSkipsPauseWithoutBlockingSessionExecution(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	session, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("background-use-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "update dorf.sessions set sandbox_last_active_at=clock_timestamp()-interval '2 minutes' where id=$1", session.ID); err != nil {
		t.Fatal(err)
	}
	useCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		done <- store.WithSandboxAwake(useCtx, session.ID, func() error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return useCtx.Err()
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var pauses int
	external := idleIntegrationExternals{pause: func() error { pauses++; return nil }}
	reconcile := func() {
		t.Helper()
		if err := core.NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(ctx, session.ID); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	if err := store.WithSessionFence(ctx, session.ID, func() error { return nil }); err != nil {
		t.Fatalf("background use blocked ordinary execution: %v", err)
	}
	cancel()
	reconcile()
	if pauses != 0 {
		t.Fatal("paused before background command cleanup finished")
	}
	release <- struct{}{}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("background operation error: %v", err)
	}
	reconcile()
	if pauses != 1 {
		t.Fatal("completed background use prevented normal idle pause")
	}
}

func TestBackgroundSandboxUseWaitsForAlreadyStartedPause(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paused, resume := make(chan struct{}), make(chan struct{})
	defer close(resume)
	done := make(chan error, 1)
	go func() {
		done <- store.WithSandboxPauseFence(ctx, "pause-first", func() error {
			close(paused)
			select {
			case <-resume:
			case <-ctx.Done():
			}
			return nil
		})
	}()
	<-paused
	useCtx, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	entered := false
	err := store.WithSandboxAwake(useCtx, "pause-first", func() error { entered = true; return nil })
	if err == nil || entered {
		t.Fatal("background use overlapped an already-running pause")
	}
	resume <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := store.WithSandboxAwake(ctx, "pause-first", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
