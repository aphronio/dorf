package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/terminal"
	"github.com/aphronio/dorf/internal/upgrade"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// TestLivePersistenceCleanup is an explicitly budgeted destructive proof. It
// creates at most two sequential E2B Sandboxes: the Session's source and a private
// inspection resource. The second resource restores one exact checkpoint but
// is never attached to or used to reopen the cleaned Session.
func TestLivePersistenceCleanup(t *testing.T) {
	if os.Getenv("DORF_LIVE_PERSISTENCE_E2B") != "1" {
		t.Skip("set DORF_LIVE_PERSISTENCE_E2B=1 with a private budget reservation")
	}
	receipt := readLivePersistenceBudget(t)
	manifest := readLivePersistenceManifest(t)
	baseConfig := readLivePersistenceConfig(t)
	fixture, err := os.ReadFile("../../scripts/runtime-upgrade/responses-fixture.py")
	if err != nil {
		t.Fatal(err)
	}
	db, store, tasks := livePersistenceDatabase(t)
	t.Cleanup(func() {
		_ = tasks.Close()
		_ = db.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profile := livePersistenceProfile(t, ctx, store, receipt.RunID+"-cleanup", manifest.Template.Reference)
	privateConfig := writeLivePersistenceConfig(t, baseConfig, profile.Ref())
	cfg := config.Config{
		PersistenceFile: privateConfig,
		Workspace:       livePersistenceWorkspace,
		AppServerPort:   livePersistencePort,
		TurnTimeout:     2 * time.Minute,
		E2BAPIKey:       os.Getenv("E2B_API_KEY"),
	}
	sandbox, err := sandboxForProfile(cfg, profile)
	if err != nil {
		t.Fatal(err)
	}
	agent := codex.Agent{Sandbox: sandbox, Port: cfg.AppServerPort, Timeout: cfg.TurnTimeout}
	proof := &livePersistenceProof{t: t, ctx: ctx, store: store, tasks: tasks, sandbox: sandbox, agent: agent, fixture: fixture}
	proof.installRuntime(profile.Ref())

	session, _, err := store.AdmitDirect(ctx, core.SessionAdmission{
		AdmissionKey: "checkpoint-cleanup-proof-" + receipt.RunID, SandboxProfile: profile.Name,
		ProviderConnection: "synthetic-local", Model: "synthetic-model", ReasoningEffort: "low", KeepRunning: true,
	}, tasks.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	proof.session = session
	proof.sandboxID = core.MainSandboxName(session.ID)
	proof.registerCleanup()
	proof.registerEventExport()

	markLivePersistenceBudgetConsumed(t, receipt)
	workerStop := proof.startWorker()
	message := proof.admit("cleanup-state", "Remember the synthetic cleanup marker CLEANUP_CHECKPOINT_154. Reply briefly without tools.")
	if execution, _ := proof.waitNativeTurn(message); message.ThreadID == "" || proof.turnOutput(execution) == "" {
		t.Fatal("source turn omitted its retained thread or substantive output")
	}
	proof.prepareUsefulState()
	proof.exec("printf 'latest-unmanaged-cleanup-edit\\n' > /workspace/cleanup-latest.txt")
	workerStop()

	publishedBeforeRoute := &atomic.Bool{}
	publishedBeforeDelete := &atomic.Bool{}
	cleanupExternals := liveCleanupExternals{
		Externals: terminal.Externals{Sandbox: sandbox, Agent: agent, Ownership: func(ctx context.Context, sandboxID string) (provider.Ownership, error) {
			owned, err := store.Sandbox(ctx, sandboxID)
			return livePersistenceOwner(owned), err
		}},
		store: store, sandboxID: proof.sandboxID,
		publishedBeforeRoute: publishedBeforeRoute, publishedBeforeDelete: publishedBeforeDelete,
	}
	baseExecution := core.NewExecutionService(store, cleanupExternals, nil, absurdruntime.RequireClaim)
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: tasks, emit: proof.emit}
	cleanupExecution := checkpointExecution{
		Execution: upgrade.Execution{ExecutionService: baseExecution, Upgrades: upgrade.Service{
			Store: store, Queue: tasks.QueueName(), Provider: string(profile.Provider), Claim: absurdruntime.RequireClaim,
		}},
		resolver: resolver,
	}
	application := coreApplication(store, tasks)
	application.CleanupRuntimes = liveCleanupRuntime{ref: profile.Ref(), execution: cleanupExecution}
	application.RegisterCleanup()

	badConfig := baseConfig.withProfile(profile.Ref())
	badConfig.ResticPath = persistence.DefaultResticPath + "-unavailable"
	rewriteLiveCleanupConfig(t, privateConfig, badConfig)
	if err := store.ScheduleCleanup(ctx, tasks.QueueName(), session.ID, ""); err != nil {
		t.Fatal(err)
	}
	failedStarted := time.Now()
	if err := tasks.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "persistence-cleanup-unavailable", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	failedDuration := time.Since(failedStarted)
	assertLiveCleanupRetained(t, ctx, store, sandbox, session.ID, proof.sandboxID)
	if publishedBeforeRoute.Load() || publishedBeforeDelete.Load() {
		t.Fatal("cleanup reached a destructive action after the unavailable backup")
	}
	if _, err := store.LastCheckpoint(ctx, proof.sandboxID); !errors.Is(err, persistence.ErrCheckpointNotFound) {
		t.Fatalf("unavailable backup published a checkpoint: %v", err)
	}

	rewriteLiveCleanupConfig(t, privateConfig, baseConfig.withProfile(profile.Ref()))
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(6 * time.Second):
	}
	cleanupStarted := time.Now()
	if err := tasks.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "persistence-cleanup-retry", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cleanupDuration := time.Since(cleanupStarted)
	cleaned, err := store.Session(ctx, session.ID)
	if err != nil || cleaned.AdmissionOpen || cleaned.CleanupState != core.CleanupComplete || cleaned.CleanedAt.IsZero() {
		t.Fatalf("cleanup Session = %#v: %v", cleaned, err)
	}
	if !publishedBeforeRoute.Load() || !publishedBeforeDelete.Load() {
		t.Fatal("cleanup did not prove checkpoint publication before route revocation and resource deletion")
	}
	checkpoint, err := store.LastCheckpoint(ctx, proof.sandboxID)
	if err != nil || !checkpoint.Cleanup || checkpoint.Repository != baseConfig.ID || len(checkpoint.SnapshotID) != 64 {
		t.Fatalf("final cleanup checkpoint = %#v: %v", checkpoint, err)
	}
	if present, err := sandbox.OwnedPresent(ctx, livePersistenceOwner(sourceSandbox(t, ctx, store, proof.sandboxID))); err != nil || present {
		t.Fatalf("cleaned source remains present=%t: %v", present, err)
	}

	inspectionDigest := sha256.Sum256([]byte("cleanup-inspection-v1\x00" + receipt.RunID))
	inspectionOwner := provider.Ownership{
		SessionID: session.ID, SandboxID: proof.sandboxID, OwnershipNonce: hex.EncodeToString(inspectionDigest[:]),
	}
	removeInspection := liveInspectionCleanup(t, sandbox, inspectionOwner)
	if err := sandbox.ReconcileOwnedCreate(ctx, inspectionOwner); err != nil {
		t.Fatal(err)
	}
	restoreStarted := time.Now()
	driver := persistence.Driver{
		Sandbox: sandbox, Repository: baseConfig.repository(), ResticPath: baseConfig.ResticPath,
		OperationTimeout: time.Duration(baseConfig.BackupTimeoutSeconds) * time.Second,
	}
	if _, err := driver.Restore(ctx, inspectionOwner, checkpoint.SnapshotID, "/"); err != nil {
		t.Fatal(err)
	}
	restoreDuration := time.Since(restoreStarted)
	assertLiveCleanupRestore(t, ctx, sandbox, inspectionOwner)
	removeInspection()

	afterInspection, err := store.Session(ctx, session.ID)
	if err != nil || afterInspection != cleaned {
		t.Fatalf("private inspection reopened or mutated cleaned Session: before=%#v after=%#v err=%v", cleaned, afterInspection, err)
	}
	t.Logf("live cleanup persistence proof passed: unavailable=%s cleanup_retry=%s restore=%s logical_added_bytes=%d",
		failedDuration, cleanupDuration, restoreDuration, proof.completedLogicalDataAdded())
}

type liveCleanupRuntime struct {
	ref       core.SandboxProfileRef
	execution core.CleanupExecution
}

func (r liveCleanupRuntime) ResolveCleanup(_ context.Context, ref core.SandboxProfileRef) (core.CleanupRuntime, error) {
	if ref != r.ref {
		return core.CleanupRuntime{}, fmt.Errorf("unexpected cleanup persistence profile")
	}
	return core.CleanupRuntime{SandboxProfile: ref, Execution: r.execution}, nil
}

type liveCleanupExternals struct {
	terminal.Externals
	store                 postgres.Store
	sandboxID             string
	publishedBeforeRoute  *atomic.Bool
	publishedBeforeDelete *atomic.Bool
}

func (e liveCleanupExternals) RouteRevoke(ctx context.Context, session core.Session, sandbox core.Sandbox, route core.Route) error {
	if err := e.requirePublished(ctx, session, sandbox); err != nil {
		return err
	}
	e.publishedBeforeRoute.Store(true)
	return e.Agent.RemoveRoute(ctx, livePersistenceOwner(sandbox))
}

func (e liveCleanupExternals) SandboxDelete(ctx context.Context, session core.Session, sandbox core.Sandbox) error {
	if err := e.requirePublished(ctx, session, sandbox); err != nil {
		return err
	}
	e.publishedBeforeDelete.Store(true)
	return e.Externals.SandboxDelete(ctx, session, sandbox)
}

func (e liveCleanupExternals) requirePublished(ctx context.Context, session core.Session, sandbox core.Sandbox) error {
	checkpoint, err := e.store.LastCheckpoint(ctx, e.sandboxID)
	if err != nil {
		return fmt.Errorf("cleanup action preceded checkpoint publication: %w", err)
	}
	if !checkpoint.Cleanup || checkpoint.SessionID != session.ID || checkpoint.SandboxID != sandbox.ID || checkpoint.ResourceID != sandbox.ResourceID {
		return fmt.Errorf("cleanup action checkpoint differs from its source boundary")
	}
	return nil
}

func rewriteLiveCleanupConfig(t *testing.T, path string, cfg checkpointConfig) {
	t.Helper()
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	next := path + ".next"
	if err := os.WriteFile(next, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

func assertLiveCleanupRetained(t *testing.T, ctx context.Context, store postgres.Store, sandbox provider.Sandbox, sessionID, sandboxID string) {
	t.Helper()
	session, err := store.Session(ctx, sessionID)
	if err != nil || session.AdmissionOpen || session.CleanupState != core.CleanupScheduled || session.CleanupAttention == "" {
		t.Fatalf("failed cleanup Session = %#v: %v", session, err)
	}
	owned := sourceSandbox(t, ctx, store, sandboxID)
	present, err := sandbox.OwnedPresent(ctx, livePersistenceOwner(owned))
	if err != nil || !present {
		t.Fatalf("failed cleanup source retained=%t: %v", present, err)
	}
	resources, err := store.SandboxResources(ctx, sessionID)
	if err != nil || len(resources) != 1 || !resources[0].DeletedAt.IsZero() {
		t.Fatalf("failed cleanup resources = %#v: %v", resources, err)
	}
}

func sourceSandbox(t *testing.T, ctx context.Context, store postgres.Store, sandboxID string) core.Sandbox {
	t.Helper()
	owned, err := store.Sandbox(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	return owned
}

func liveInspectionCleanup(t *testing.T, sandbox provider.Sandbox, owner provider.Ownership) func() {
	t.Helper()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if err := sandbox.DeleteOwned(cleanupCtx, owner); err != nil {
				t.Errorf("delete private inspection resource: %v", err)
			}
		})
	}
	t.Cleanup(cleanup)
	return cleanup
}

func assertLiveCleanupRestore(t *testing.T, ctx context.Context, sandbox provider.Sandbox, owner provider.Ownership) {
	t.Helper()
	result, err := sandbox.Exec(ctx, owner, nil, "bash", "-c", `set -eu
cd /workspace
test "$(cat cleanup-latest.txt)" = latest-unmanaged-cleanup-edit
test "$(cat tracked.txt)" = "committed
working-tree-edit"
test "$(cat untracked.txt)" = untracked
git fsck --no-dangling >/dev/null
test "$(cat /root/.codex/AGENTS.md)" = "Persistent synthetic instruction."
test ! -e /root/.codex/config.toml
test ! -e /root/.config/dorf/provider-route.key
python3 - <<'PY'
import sqlite3
db=sqlite3.connect('/workspace/.dorf-proof/useful.sqlite')
assert db.execute('pragma integrity_check').fetchone()[0]=='ok'
assert db.execute('select value from evidence').fetchone()[0]=='retained-wal-row'
PY`)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("exact cleanup snapshot restore failed (exit=%d): %v: %s", result.ExitCode, err, strings.TrimSpace(result.Stderr))
	}
}
