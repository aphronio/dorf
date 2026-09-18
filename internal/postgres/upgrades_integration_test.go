package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/aphronio/dorf/internal/upgrade"
)

type upgradeDriver struct {
	replace           bool
	failVerify        bool
	loseRestore       bool
	checkpoint        provider.Checkpoint
	versions          map[string]string
	data              map[string]string
	creates           int
	deleted           map[string]bool
	checkpointDeleted bool
}

func (d *upgradeDriver) InspectPackage(context.Context, core.Sandbox, upgrade.Request) (string, error) {
	return "0.154.0", nil
}
func (d *upgradeDriver) Quiesce(context.Context, core.Sandbox, []core.AgentRun) error { return nil }
func (d *upgradeDriver) Capture(_ context.Context, s core.Sandbox, key string) (provider.Checkpoint, error) {
	if d.checkpoint.Reference == "" {
		d.checkpoint = provider.Checkpoint{Key: key, Reference: "checkpoint-" + key, SourceID: s.ProviderID}
	}
	return d.checkpoint, nil
}
func (d *upgradeDriver) Activate(_ context.Context, s core.Sandbox, r upgrade.Request) error {
	d.versions[s.ResourceID] = r.Version
	d.data[s.ResourceID] = "new-incompatible-state"
	return nil
}
func (d *upgradeDriver) ReplacesResource() bool { return d.replace }
func (d *upgradeDriver) Restore(_ context.Context, source, destination core.Sandbox, _ provider.Checkpoint) (string, error) {
	if d.replace && d.versions[destination.ResourceID] == "" {
		d.creates++
	}
	d.versions[destination.ResourceID] = "0.154.0"
	d.data[destination.ResourceID] = "original-state"
	id := source.ProviderID
	if d.replace {
		id = "provider-replacement"
	}
	if d.loseRestore {
		d.loseRestore = false
		return "", errors.New("lost restore acknowledgement")
	}
	return id, nil
}
func (d *upgradeDriver) Verify(_ context.Context, s core.Sandbox, version string, runs []core.AgentRun) error {
	if len(runs) != 1 || runs[0].ThreadID != "retained-thread" {
		return errors.New("lost conversation")
	}
	if version == "0.155.0" && d.failVerify {
		return errors.New("new runner cannot read session")
	}
	if d.versions[s.ResourceID] != version {
		return errors.New("wrong version")
	}
	return nil
}
func (d *upgradeDriver) DeleteResource(_ context.Context, s core.Sandbox) error {
	d.deleted[s.ResourceID] = true
	return nil
}
func (d *upgradeDriver) DeleteCheckpoint(context.Context, core.Sandbox, provider.Checkpoint) error {
	d.checkpointDeleted = true
	return nil
}

func TestSandboxUpgradeRecoveryKeepsCustodyAndQueuedInput(t *testing.T) {
	for _, tc := range []struct {
		name              string
		replace, rollback bool
	}{{"upgrade", false, false}, {"incus-rollback", false, true}, {"e2b-replacement", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, client := testDatabase(t)
			ctx := context.Background()
			job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{AdmissionKey: fmt.Sprintf("upgrade-%s-%d", tc.name, time.Now().UnixNano()), SandboxProfile: "incus", ProviderConnection: "primary", Model: "model-test", ReasoningEffort: "low"})
			if err != nil {
				t.Fatal(err)
			}
			owned, err := store.Sandbox(ctx, core.MainSandboxName(job.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.BindSandboxResource(ctx, owned, "provider-original"); err != nil {
				t.Fatal(err)
			}
			current, err := nextDelivery(ctx, store, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PrepareAgentRun(ctx, current.AgentRun.ID, "codex", ""); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, current.AgentRun.ID, "codex", "retained-thread", "retained-turn", "completed"); err != nil {
				t.Fatal(err)
			}
			request := upgrade.Request{ID: "upgrade-" + strings.ReplaceAll(job.ID, "_", "-"), JobID: job.ID, SandboxID: owned.ID, PackagePath: "/nix/store/" + strings.Repeat("a", 32) + "-codex-0.155.0", Version: "0.155.0"}
			// Keep provider checkpoint keys within the shared 63-character limit.
			request.ID = "upgrade-" + job.ID[len(job.ID)-24:]
			receipt, err := store.RequestSandboxUpgrade(ctx, client.QueueName(), request)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := store.RequestSandboxUpgrade(ctx, client.QueueName(), request)
			if err != nil || replay.RequestedAt != receipt.RequestedAt {
				t.Fatalf("request replay: %v", err)
			}
			changed := request
			changed.Version = "0.156.0"
			if _, err := store.RequestSandboxUpgrade(ctx, client.QueueName(), changed); err == nil {
				t.Fatal("changed request accepted")
			}
			if err := store.ReleaseSandboxDelivery(ctx, client.QueueName(), job.ID, owned.ID, request.ID); err == nil {
				t.Fatal("generic release bypassed verification")
			}
			if err := store.FinishSandboxUpgrade(ctx, client.QueueName(), receipt); err == nil {
				t.Fatal("unverified release accepted")
			}
			queued, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: owned.ID, FromKind: core.MessageFromHuman, FromID: "held-input", Input: "continue after upgrade", Intent: core.MessageAuto})
			if err != nil {
				t.Fatal(err)
			}
			driver := &upgradeDriver{replace: tc.replace, failVerify: tc.rollback, loseRestore: tc.replace, versions: map[string]string{owned.ResourceID: "0.154.0"}, data: map[string]string{owned.ResourceID: "original-state"}, deleted: map[string]bool{}}
			var events []telemetry.Event
			service := upgrade.Service{Store: store, Driver: driver, Queue: client.QueueName(), Claim: func(context.Context) error { return nil }, Emit: func(event telemetry.Event) { events = append(events, event) }}
			lostResponse := false
			for range 20 {
				records, err := store.JobUpgrades(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				receipt = records[0]
				if !receipt.VerifiedAt.IsZero() && ((!tc.replace && !receipt.CheckpointDeletedAt.IsZero()) || (tc.replace && driver.deleted[owned.ResourceID])) {
					break
				}
				_, err = service.Reconcile(ctx, job.ID)
				if err != nil {
					if !tc.replace || lostResponse {
						t.Fatal(err)
					}
					lostResponse = true
				}
				// A new executor sees only retained facts, never an in-memory step index.
				service.Store = postgres.Store{DB: store.DB}
				selected, err := store.AgentMessage(ctx, job.ID)
				if err != nil || selected != nil {
					t.Fatalf("hold leaked delivery: %v", err)
				}
			}
			if receipt.VerifiedAt.IsZero() {
				t.Fatal("verification did not converge")
			}
			service.Queue = "missing_queue"
			if _, err := service.Reconcile(ctx, job.ID); err == nil {
				t.Fatal("release succeeded without wake")
			}
			held, _ := store.SandboxDeliveryHeld(ctx, owned.ID)
			before, _ := store.Sandbox(ctx, owned.ID)
			if !held || before.ResourceID != owned.ResourceID {
				t.Fatal("failed wake partially switched custody")
			}
			service.Queue = client.QueueName()
			if _, err := service.Reconcile(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			after, err := store.Sandbox(ctx, owned.ID)
			if err != nil {
				t.Fatal(err)
			}
			history, _ := store.JobUpgrades(ctx, job.ID)
			want := "upgraded"
			if tc.rollback {
				want = "rolled_back"
			}
			if history[0].Outcome() != want {
				t.Fatalf("outcome=%s", history[0].Outcome())
			}
			if tc.replace && (after.ResourceID == owned.ResourceID || after.OwnershipNonce == owned.OwnershipNonce || after.ProviderID != "provider-replacement" || driver.creates != 1 || !lostResponse) {
				t.Fatal("replacement did not retain exact distinct custody")
			}
			if tc.rollback && driver.data[after.ResourceID] != "original-state" {
				t.Fatal("rollback did not restore state")
			}
			if tc.replace && driver.checkpointDeleted {
				t.Fatal("deleted replacement's backing checkpoint")
			}
			next, err := store.AgentMessage(ctx, job.ID)
			if err != nil || next == nil || next.MessageID != queued.Message.ID {
				t.Fatalf("queued input was not resumed: %v", err)
			}
			execution, err := store.AgentMessageExecution(ctx, queued.Message.ID)
			if err != nil || execution.Sandbox.ResourceID != after.ResourceID {
				t.Fatalf("queued input uses stale resource: %v", err)
			}
			if len(events) == 0 || events[0].Attributes["dorf.upgrade_id"] != request.ID {
				t.Fatal("missing correlated diagnostics")
			}
			if _, err := store.RequestSandboxUpgrade(ctx, client.QueueName(), request); err != nil {
				t.Fatal(err)
			}
			held, _ = store.SandboxDeliveryHeld(ctx, owned.ID)
			if held {
				t.Fatal("replay reopened completed upgrade")
			}
		})
	}
}
