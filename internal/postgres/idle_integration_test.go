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

func (e idleIntegrationExternals) SandboxPause(context.Context, core.Job, core.Sandbox) error {
	return e.pause()
}

func TestIdlePolicyAdmissionAndReplay(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	service := direct.NewAdmissionService(store, client.QueueName(), providerCheck{})
	for _, keep := range []bool{false, true} {
		request := direct.AdmissionRequest{KeepRunning: keep, AdmissionKey: fmt.Sprintf("idle-policy-%t-%d", keep, time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"}
		job, created, err := service.Admit(ctx, request)
		if err != nil || !created || job.KeepRunning != keep {
			t.Fatalf("admit=%+v %v", job, err)
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

func TestIdlePauseFenceOrdersNewMessageAndRetry(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	job, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("idle-race-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "update dorf.jobs set sandbox_last_active_at=clock_timestamp()-interval '2 minutes' where id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	external := idleIntegrationExternals{pause: func() error {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	execution := core.NewExecutionService(store, external, nil, nil)
	paused := make(chan error, 1)
	go func() { paused <- execution.ReconcileIdleSandboxes(ctx, job.ID) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	admitted := make(chan error, 1)
	go func() {
		_, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "new-turn", Input: "continue", Intent: core.MessageFollow})
		admitted <- err
	}()
	// Admission can acknowledge while a provider call is in progress. Native
	// delivery is fenced; the queued message must invalidate the next idle check.
	if err := <-admitted; err != nil {
		t.Fatal(err)
	}
	retried := make(chan error, 1)
	go func() {
		retried <- core.NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(ctx, job.ID)
	}()
	select {
	case err := <-retried:
		t.Fatalf("idle retry bypassed the effect fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-paused; err != nil {
		t.Fatal(err)
	}
	if err := <-retried; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("new work was paused: calls=%d", calls.Load())
	}
}

func TestSandboxIdleGracePersistsAcrossRestartAndInterruptedActivity(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	job, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("idle-grace-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	external := idleIntegrationExternals{pause: func() error { calls++; return nil }}
	reconcile := func() {
		t.Helper()
		if err := core.NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	activityTime := func() time.Time {
		t.Helper()
		var at time.Time
		if err := db.QueryRowContext(ctx, "select sandbox_last_active_at from dorf.jobs where id=$1", job.ID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	age := func(seconds int) {
		t.Helper()
		if _, err := db.ExecContext(ctx, "update dorf.jobs set sandbox_last_active_at=clock_timestamp()-make_interval(secs=>$2) where id=$1", job.ID, seconds); err != nil {
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
	if err := store.WithJobFence(ctx, job.ID, func() error { return store.BeginSandboxActivity(ctx, job.ID) }); err != nil {
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
	if err := store.WithJobFence(ctx, job.ID, func() error {
		return core.WithSandboxActivity(ctx, store, job.ID, func() error { return failure })
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
	job, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("idle-cancel-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "update dorf.jobs set sandbox_last_active_at=clock_timestamp()-interval '2 minutes' where id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- store.WithJobFence(requestCtx, job.ID, func() error {
			return core.WithSandboxActivity(requestCtx, store, job.ID, func() error {
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
		paused <- core.NewExecutionService(store, external, nil, nil).ReconcileIdleSandboxes(ctx, job.ID)
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
	if err := db.QueryRowContext(ctx, "select sandbox_last_active_at > clock_timestamp()-interval '5 seconds' from dorf.jobs where id=$1", job.ID).Scan(&recent); err != nil || !recent {
		t.Fatalf("finish was not durable: recent=%v error=%v", recent, err)
	}
}

func TestIdleGraceExcludesEveryUnsettledRunState(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	job, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("idle-run-states-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "run", Input: "work", Intent: core.MessageFollow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "update dorf.jobs set sandbox_last_active_at=clock_timestamp()-interval '2 minutes' where id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "submitting", "active", "uncertain", "completed", "failed", "interrupted"} {
		if _, err := db.ExecContext(ctx, "update dorf.agent_runs set state=$2 where message_id=$1", admitted.Message.ID, state); err != nil {
			t.Fatal(err)
		}
		eligible, err := store.SandboxIdleFor(ctx, job.ID, time.Minute)
		want := state == "completed" || state == "failed" || state == "interrupted"
		if err != nil || eligible != want {
			t.Fatalf("state=%s idle=%v want=%v err=%v", state, eligible, want, err)
		}
		if want {
			work, err := store.AgentMessage(ctx, job.ID)
			if err != nil || work != nil {
				t.Fatalf("idle poll selected settled state=%s work=%+v err=%v", state, work, err)
			}
		}
	}
}
