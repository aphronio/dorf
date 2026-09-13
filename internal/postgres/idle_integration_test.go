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
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	job, _, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{AdmissionKey: fmt.Sprintf("idle-race-%d", time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
	if err != nil {
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
