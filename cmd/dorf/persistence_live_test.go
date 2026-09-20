package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/aphronio/dorf/internal/terminal"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	livePersistenceWorkspace = "/workspace"
	livePersistencePort      = 8755
	livePersistenceTimeout   = 10 * time.Minute
)

// TestLivePersistenceRecovery is an explicitly budgeted destructive proof. It
// creates at most two sequential E2B Sandboxes: the source and its replacement.
// The model is a local synthetic Responses fixture; no user data or model key
// enters either Sandbox.
func TestLivePersistenceRecovery(t *testing.T) {
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
		tasks.Close()
		db.Close()
	})

	// Every provider call can renew E2B's 600-second Sandbox timeout. Stop all
	// create-capable work after eight minutes; cleanup gets a separate 90-second
	// delete-only context below.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profile := livePersistenceProfile(t, ctx, store, receipt.RunID, manifest.Template.Reference)
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
		AdmissionKey: "checkpoint-proof-" + receipt.RunID, SandboxProfile: profile.Name,
		ProviderConnection: "synthetic-local", Model: "synthetic-model", ReasoningEffort: "low", KeepRunning: true,
	}, tasks.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	proof.session = session
	proof.sandboxID = core.MainSandboxName(session.ID)
	proof.registerEventExport()
	proof.registerCleanup()

	markLivePersistenceBudgetConsumed(t, receipt)
	workerStop := proof.startWorker()
	initial := proof.admit("initial", "Remember the synthetic marker CHECKPOINT_CONTEXT_154. Reply briefly without tools.")
	initialExecution, initialLatency := proof.waitNativeTurn(initial)
	threadID := initial.ThreadID
	if threadID == "" || proof.turnOutput(initialExecution) == "" {
		t.Fatal("initial Codex turn omitted its retained thread or substantive output")
	}
	proof.verifyGuestAllocation()
	proof.prepareUsefulState()
	proof.preflightNativeCapture(baseConfig.AdditionalPaths)

	service := proof.checkpointService(cfg)
	formatTurnTiming := func(acceptance, observed time.Duration) string {
		return fmt.Sprintf("{native_acceptance=%s,observed_completion=%s}", acceptance, observed)
	}
	disabledTimings := make([]string, 0, 3)
	var disabledDelta livePersistenceResourceDelta
	for index := 0; index < 3; index++ {
		proof.waitIdle()
		resources := proof.resourceSample()
		admissionStarted := time.Now()
		baseline := proof.admit(fmt.Sprintf("latency-disabled-%d", index), "Reply briefly without tools.")
		admissionLatency := time.Since(admissionStarted)
		_, observedLatency := proof.waitNativeTurn(baseline)
		disabledTimings = append(disabledTimings, formatTurnTiming(admissionLatency, observedLatency))
		disabledDelta = disabledDelta.add(resources.delta(proof.resourceSample()))
	}
	enabledTimings := make([]string, 0, 3)
	var cancelledDelta livePersistenceResourceDelta
	for index := 0; index < 3; index++ {
		proof.createSlowCaptureInput(index)
		proof.waitIdle()
		before, err := store.ListCheckpoints(ctx, proof.sandboxID)
		if err != nil {
			t.Fatal(err)
		}
		resources := proof.resourceSample()
		captureDone := make(chan error, 1)
		go func() {
			_, captureErr := service.Capture(ctx, proof.sandboxID, false)
			captureDone <- captureErr
		}()
		proof.waitForResticBackup(captureDone)
		enabledStarted := time.Now()
		enabled := proof.admit(fmt.Sprintf("latency-enabled-%d", index), "Reply briefly while the idle checkpoint is being cancelled. Do not use tools.")
		admissionLatency := time.Since(enabledStarted)
		_, observedLatency := proof.waitNativeTurn(enabled)
		select {
		case captureErr := <-captureDone:
			if !errors.Is(captureErr, persistence.ErrCheckpointSuperseded) {
				t.Fatalf("checkpoint interrupted by new input returned %v", captureErr)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("cancelled checkpoint did not confirm remote restic termination")
		}
		after, err := store.ListCheckpoints(ctx, proof.sandboxID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) {
			t.Fatal("cancelled checkpoint published a recovery point")
		}
		enabledTimings = append(enabledTimings, formatTurnTiming(admissionLatency, observedLatency))
		cancelledDelta = cancelledDelta.add(resources.delta(proof.resourceSample()))
		proof.removeSlowCaptureInput(index)
	}
	proof.requireCancellationEvidence(3)
	t.Logf("small live latency smoke samples after equal idle periods (three pairs, no regression inference): checkpoint_disabled=[%s] active_checkpoint_cancel=[%s]",
		strings.Join(disabledTimings, ", "), strings.Join(enabledTimings, ", "))
	t.Logf("guest resource smoke sample before final capture: disabled={%s} cancelled_backups={%s}", disabledDelta, cancelledDelta)
	proof.waitIdle()
	expectedBoundary, err := store.Boundary(ctx, proof.sandboxID, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LastCheckpoint(ctx, proof.sandboxID); !errors.Is(err, persistence.ErrCheckpointNotFound) {
		t.Fatalf("cancelled checkpoint unexpectedly became authoritative: %v", err)
	}
	successResources := proof.resourceSample()
	var checkpoint persistence.Checkpoint
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: tasks, emit: proof.emit}
	if err := runWithCheckpoints(ctx, resolver, func(foregroundCtx context.Context) error {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			published, err := store.LastCheckpoint(foregroundCtx, proof.sandboxID)
			if err == nil {
				checkpoint = published
				return nil
			}
			if !errors.Is(err, persistence.ErrCheckpointNotFound) {
				return err
			}
			select {
			case <-foregroundCtx.Done():
				return foregroundCtx.Err()
			case <-ticker.C:
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	successDelta := successResources.delta(proof.resourceSample())
	if checkpoint.CaptureBoundary != expectedBoundary {
		t.Fatal("automatic idle checkpoint published a different execution boundary")
	}
	if len(checkpoint.SnapshotID) != 64 || checkpoint.Repository != baseConfig.ID {
		t.Fatal("idle checkpoint omitted its exact immutable storage identity")
	}

	workerStop()
	request := persistence.RecoveryRequest{
		ID: "recover-" + receipt.RunID, SessionID: session.ID, SandboxID: proof.sandboxID,
		Repository: checkpoint.Repository, SnapshotID: checkpoint.SnapshotID,
	}
	if _, err := store.RequestCheckpointRecovery(ctx, tasks.QueueName(), request); err != nil {
		t.Fatal(err)
	}
	source, err := store.Sandbox(ctx, proof.sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sandbox.DeleteOwned(ctx, livePersistenceOwner(source)); err != nil {
		t.Fatal(err)
	}

	baseRecovery := checkpointRecovery{
		capture: checkpointCapture{config: baseConfig.withProfile(profile.Ref()), store: store, sandbox: sandbox, agent: agent},
	}
	recoveryDriver := newLivePersistenceGatewayRecovery(t, baseRecovery, fixture)
	recovery := persistence.RecoveryService{
		Store: store, Driver: recoveryDriver,
		Queue: tasks.QueueName(), Claim: func(context.Context) error { return nil },
	}
	for attempt := 0; attempt < 8; attempt++ {
		progressed, reconcileErr := recovery.Reconcile(ctx, session.ID)
		if errors.Is(reconcileErr, errLivePersistenceLostVerificationReceipt) && !progressed {
			continue
		}
		if reconcileErr != nil {
			t.Fatalf("recovery attempt %d: %v", attempt+1, reconcileErr)
		}
		if !progressed {
			break
		}
	}
	active, err := store.Sandbox(ctx, proof.sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if active.ResourceID == source.ResourceID || active.ProviderID == "" || active.ProviderID == source.ProviderID {
		t.Fatal("recovery did not adopt a fresh provider resource")
	}
	proof.verifyGuestAllocation()
	proof.verifyRestoredUsefulState()

	workerStop = proof.startWorker()
	queued := proof.admit("queued-recovery", "Reply with the synthetic marker remembered before recovery. Do not use tools.")
	recoveredExecution, recoveredLatency := proof.waitNativeTurn(queued)
	workerStop()
	if queued.ThreadID != threadID || proof.turnOutput(recoveredExecution) == "" {
		t.Fatal("replacement did not continue the original Codex thread with substantive output")
	}
	proof.requireRestoredConversation("CHECKPOINT_CONTEXT_154")
	resources, err := store.SandboxResources(ctx, session.ID)
	if err != nil || len(resources) != 2 {
		t.Fatalf("proof created %d resource records, want exactly source and replacement: %v", len(resources), err)
	}
	t.Logf("live persistence proof passed: initial=%s checkpoint_disabled=[%s] active_checkpoint_cancel=[%s] recovered=%s logical_added_bytes=%d",
		initialLatency, strings.Join(disabledTimings, ", "), strings.Join(enabledTimings, ", "), recoveredLatency, proof.completedLogicalDataAdded())
	t.Logf("guest deltas: disabled={%s} cancelled_backups={%s} successful_backup={%s}", disabledDelta, cancelledDelta, successDelta)
	t.Log("scope: exact Codex 0.154 native/workspace recovery, direct R2 restic, cancellation, PostgreSQL adoption, and production Gateway key renewal passed; model inference used the deterministic local fixture")
}

type livePersistenceBudget struct {
	RunID        string  `json:"run_id"`
	MaxVMs       int     `json:"max_vms"`
	MaxVCPU      int     `json:"max_vcpu"`
	MaxMemoryMiB int64   `json:"max_memory_mib"`
	TTLSeconds   int     `json:"ttl_seconds"`
	ReservedUSD  float64 `json:"reserved_usd"`
}

func readLivePersistenceBudget(t *testing.T) livePersistenceBudget {
	t.Helper()
	path := os.Getenv("DORF_LIVE_BUDGET_RESERVATION")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("DORF_LIVE_BUDGET_RESERVATION must name one absolute private receipt")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		t.Fatal("live budget reservation must be a private regular file of at most 4 KiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("live budget reservation is unreadable")
	}
	var receipt livePersistenceBudget
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		t.Fatal("live budget reservation is invalid")
	}
	if receipt.RunID == "" || len(receipt.RunID) > 80 || strings.TrimSpace(receipt.RunID) != receipt.RunID ||
		receipt.MaxVMs != 2 || receipt.MaxVCPU != 8 || receipt.MaxMemoryMiB != 8192 || receipt.TTLSeconds != 600 ||
		// Eight minutes of create-capable work plus the maximum 600-second
		// provider timeout residual costs at most $0.15984 for two 4-vCPU VMs.
		receipt.ReservedUSD < 0.15984 || receipt.ReservedUSD > 0.1776 || time.Since(info.ModTime()) > time.Duration(receipt.TTLSeconds)*time.Second {
		t.Fatal("live budget reservation is expired or exceeds the proof bounds")
	}
	return receipt
}

// consumeLivePersistenceBudget is retained for focused live proofs that have
// no separate local preflight phase.
func consumeLivePersistenceBudget(t *testing.T) livePersistenceBudget {
	t.Helper()
	receipt := readLivePersistenceBudget(t)
	markLivePersistenceBudgetConsumed(t, receipt)
	return receipt
}

func markLivePersistenceBudgetConsumed(t *testing.T, expected livePersistenceBudget) {
	t.Helper()
	if observed := readLivePersistenceBudget(t); observed != expected {
		t.Fatal("live budget reservation changed before provider creation")
	}
	path := os.Getenv("DORF_LIVE_BUDGET_RESERVATION")
	consumed, err := os.OpenFile(path+".consumed", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("live budget reservation was already consumed")
	}
	if _, err := consumed.WriteString(expected.RunID + "\n"); err != nil {
		consumed.Close()
		t.Fatal(err)
	}
	if err := consumed.Close(); err != nil {
		t.Fatal(err)
	}
}

type livePersistenceManifest struct {
	Template struct {
		Reference string `json:"reference"`
		BuildID   string `json:"build_id"`
	} `json:"template"`
	Profile struct {
		Workspace     string            `json:"workspace"`
		DefaultUser   string            `json:"default_user"`
		PackageInputs map[string]string `json:"package_inputs"`
	} `json:"profile"`
	SourceCommit string `json:"source_commit"`
	SourceDirty  bool   `json:"source_dirty"`
}

func readLivePersistenceManifest(t *testing.T) livePersistenceManifest {
	t.Helper()
	path := os.Getenv("DORF_E2B_PROFILE_MANIFEST")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatal("DORF_E2B_PROFILE_MANIFEST must name the exact absolute image-build receipt")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest livePersistenceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Template.Reference == "" || manifest.Template.BuildID == "" || len(manifest.SourceCommit) != 40 ||
		manifest.Profile.PackageInputs["scripts/sandbox/packages/workstation.nix"] == "" ||
		manifest.Profile.Workspace != livePersistenceWorkspace || manifest.Profile.DefaultUser != "root" {
		t.Fatal("image-build receipt is incomplete or uses the wrong guest contract")
	}
	if manifest.SourceDirty {
		t.Log("image-build receipt records exact dirty source inputs; this proof does not qualify the artifact as a release")
	}
	return manifest
}

func readLivePersistenceConfig(t *testing.T) checkpointConfig {
	t.Helper()
	path := os.Getenv("DORF_PERSISTENCE_CONFIG")
	if path == "" {
		t.Fatal("DORF_PERSISTENCE_CONFIG is required")
	}
	cfg, err := readCheckpointConfig(path)
	if err != nil || cfg == nil {
		t.Fatalf("read private persistence configuration: %v", err)
	}
	if cfg.ResticPath != "/nix/var/nix/profiles/dorf-tools/bin/restic" {
		t.Fatal("live proof requires restic from the pinned dorf-tools profile")
	}
	return *cfg
}

func (c checkpointConfig) withProfile(ref core.SandboxProfileRef) checkpointConfig {
	c.ProfileName, c.ProfileRevision = ref.Name, ref.Revision
	c.IdleDelaySeconds = 5
	c.BackupTimeoutSeconds = 120
	return c
}

func writeLivePersistenceConfig(t *testing.T, base checkpointConfig, ref core.SandboxProfileRef) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	data, err := json.Marshal(base.withProfile(ref))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func livePersistenceDatabase(t *testing.T) (*sql.DB, postgres.Store, *absurd.Client) {
	t.Helper()
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" || os.Getenv("E2B_API_KEY") == "" {
		t.Fatal("DORF_TEST_DATABASE_URL and E2B_API_KEY are required")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.Store{DB: db}
	if err := store.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	queue := fmt.Sprintf("dorf_persistence_proof_%d", time.Now().UnixNano())
	tasks, err := absurd.New(absurd.Options{DB: db, QueueName: queue})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := tasks.CreateQueue(context.Background(), queue); err != nil {
		tasks.Close()
		db.Close()
		t.Fatal(err)
	}
	return db, store, tasks
}

func livePersistenceProfile(t *testing.T, ctx context.Context, store postgres.Store, runID, artifact string) core.SandboxProfile {
	t.Helper()
	digest := sha256.Sum256([]byte(runID))
	profile, created, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: "checkpoint-proof-" + hex.EncodeToString(digest[:6]), Provider: core.SandboxProviderE2B,
		Harness: codex.Harness, Artifact: artifact, E2BGatewayURL: "https://models.example.invalid/v1",
		E2BSandboxTimeout: livePersistenceTimeout, E2BAllowInternet: true,
	})
	if err != nil || !created {
		t.Fatalf("create unique live profile: created=%t err=%v", created, err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, profile.Name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex-cli 0.154.0")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	profile, err = store.ActiveSandboxProfile(ctx, profile.Name)
	if err != nil || !profile.BaseVerified() {
		t.Fatalf("activate live proof profile: %v", err)
	}
	return profile
}

type livePersistenceProof struct {
	reader    controlreader.Service
	t         *testing.T
	ctx       context.Context
	store     postgres.Store
	tasks     *absurd.Client
	sandbox   provider.Sandbox
	agent     codex.Agent
	fixture   []byte
	session   core.Session
	sandboxID string
	eventsMu  sync.Mutex
	events    []telemetry.Event
}

type livePersistenceResourceSample struct {
	CPUUsec    uint64
	MemoryUsed uint64
	RXBytes    uint64
	TXBytes    uint64
}

type livePersistenceResourceDelta struct {
	CPUUsec        uint64
	MemoryUsedFrom uint64
	MemoryUsedTo   uint64
	RXBytes        uint64
	TXBytes        uint64
}

func (before livePersistenceResourceSample) delta(after livePersistenceResourceSample) livePersistenceResourceDelta {
	return livePersistenceResourceDelta{
		CPUUsec: monotonicDelta(before.CPUUsec, after.CPUUsec), MemoryUsedFrom: before.MemoryUsed, MemoryUsedTo: after.MemoryUsed,
		RXBytes: monotonicDelta(before.RXBytes, after.RXBytes), TXBytes: monotonicDelta(before.TXBytes, after.TXBytes),
	}
}

func (d livePersistenceResourceDelta) add(other livePersistenceResourceDelta) livePersistenceResourceDelta {
	from := d.MemoryUsedFrom
	if from == 0 {
		from = other.MemoryUsedFrom
	}
	return livePersistenceResourceDelta{
		CPUUsec: d.CPUUsec + other.CPUUsec, MemoryUsedFrom: from, MemoryUsedTo: other.MemoryUsedTo,
		RXBytes: d.RXBytes + other.RXBytes, TXBytes: d.TXBytes + other.TXBytes,
	}
}

func (d livePersistenceResourceDelta) String() string {
	return fmt.Sprintf("cpu_usec=%d memory_used=%d->%d rx_bytes=%d tx_bytes=%d", d.CPUUsec, d.MemoryUsedFrom, d.MemoryUsedTo, d.RXBytes, d.TXBytes)
}

func monotonicDelta(before, after uint64) uint64 {
	if after < before {
		return 0
	}
	return after - before
}

type livePersistenceRuntime struct {
	native    core.NativeSession
	ref       core.SandboxProfileRef
	execution interface {
		core.Execution
		direct.Execution
	}
}

func (r livePersistenceRuntime) ResolveDirect(_ context.Context, ref core.SandboxProfileRef) (direct.Runtime, error) {
	if ref != r.ref {
		return direct.Runtime{}, fmt.Errorf("unexpected live persistence profile")
	}
	return direct.Runtime{SandboxProfile: ref, Execution: r.execution}, nil
}

func (r livePersistenceRuntime) ResolveSandbox(_ context.Context, ref core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if ref != r.ref {
		return core.SandboxRuntime{}, fmt.Errorf("unexpected live persistence profile")
	}
	return core.SandboxRuntime{SandboxProfile: ref, Execution: r.execution, Native: r.native}, nil
}

type livePersistenceExternals struct {
	terminal.Externals
	fixture []byte
}

func (e livePersistenceExternals) RouteCreate(ctx context.Context, session core.Session, sandbox core.Sandbox, route core.Route) error {
	if session.ID != sandbox.SessionID || route.SandboxID != sandbox.ID || route.ID == "" {
		return fmt.Errorf("synthetic route has the wrong owner")
	}
	return installLivePersistenceFixture(ctx, e.Sandbox, livePersistenceOwner(sandbox), e.fixture, "synthetic-source-route")
}

func (p *livePersistenceProof) installRuntime(ref core.SandboxProfileRef) {
	p.t.Helper()
	owner := func(ctx context.Context, id string) (provider.Ownership, error) {
		owned, err := p.store.Sandbox(ctx, id)
		return livePersistenceOwner(owned), err
	}
	externals := livePersistenceExternals{Externals: terminal.Externals{Sandbox: p.sandbox, Agent: p.agent, Ownership: owner}, fixture: p.fixture}
	execution := core.NewExecutionService(p.store, externals, nil, absurdruntime.RequireClaim)
	p.reader = controlreader.Service{Store: p.store, Runtimes: livePersistenceRuntime{ref: ref, execution: execution, native: externals.Externals}}
	direct.Register(core.Application{Store: p.store, Tasks: p.tasks, SandboxRuntimes: livePersistenceRuntime{ref: ref, execution: execution, native: externals.Externals}}, p.store,
		livePersistenceRuntime{ref: ref, execution: execution, native: externals.Externals})
}

func (p *livePersistenceProof) startWorker() func() {
	p.t.Helper()
	ctx, cancel := context.WithCancel(p.ctx)
	done := make(chan error, 1)
	go func() {
		done <- p.tasks.RunWorker(ctx, absurd.WorkerOptions{WorkerID: "persistence-live", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 1, PollInterval: 20 * time.Millisecond})
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				p.t.Errorf("stop live worker: %v", err)
			}
		})
	}
	p.t.Cleanup(stop)
	return stop
}

func (p *livePersistenceProof) admit(key, input string) core.NativeAcknowledgement {
	p.t.Helper()
	for {
		ack, err := p.reader.SubmitEvent(p.ctx, p.session.ID, core.NativeEvent{Type: core.InputMessage, ClientID: key, Text: input})
		if err == nil {
			return ack
		}
		if !errors.Is(err, core.ErrNativeUnavailable) && !errors.Is(err, controlreader.ErrUnavailable) {
			p.t.Fatal(err)
		}
		select {
		case <-p.ctx.Done():
			p.t.Fatal(p.ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (p *livePersistenceProof) waitNativeTurn(ack core.NativeAcknowledgement) (core.HarnessTurn, time.Duration) {
	p.t.Helper()
	started := time.Now()
	for {
		history, err := p.reader.ReadNativeTurns(p.ctx, p.session.ID)
		if err != nil {
			p.t.Fatal(err)
		}
		if history.ThreadID != ack.ThreadID {
			p.t.Fatal("native Thread changed after acceptance")
		}
		for _, turn := range history.Turns {
			if turn.ID == ack.TurnID && turn.Terminal() {
				if turn.Status != "completed" {
					p.t.Fatalf("native status=%s", turn.Status)
				}
				return turn, time.Since(started)
			}
		}
		select {
		case <-p.ctx.Done():
			p.t.Fatal(p.ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (p *livePersistenceProof) turnOutput(turn core.HarnessTurn) string {
	return strings.TrimSpace(turn.Output)
}

func (p *livePersistenceProof) exec(command string, args ...string) string {
	p.t.Helper()
	return p.execLabeled("guest proof command", command, args...)
}

func (p *livePersistenceProof) execLabeled(label, command string, args ...string) string {
	p.t.Helper()
	owned, err := p.store.Sandbox(p.ctx, p.sandboxID)
	if err != nil {
		p.t.Fatal(err)
	}
	result, err := p.sandbox.Exec(p.ctx, livePersistenceOwner(owned), nil, append([]string{"bash", "-c", command, "persistence-proof"}, args...)...)
	if err != nil || result.ExitCode != 0 {
		p.t.Fatalf("%s failed (exit=%d): %v", label, result.ExitCode, err)
	}
	return strings.TrimSpace(result.Stdout)
}

func (p *livePersistenceProof) resourceSample() livePersistenceResourceSample {
	p.t.Helper()
	values := strings.Fields(p.execLabeled("sample guest resources", `python3 - <<'PY'
import os
from pathlib import Path
cpu_fields=Path('/proc/stat').read_text().splitlines()[0].split()[1:]
busy_ticks=sum(int(cpu_fields[index]) for index in (0,1,2,5,6))
cpu=busy_ticks*1_000_000//os.sysconf('SC_CLK_TCK')
memory={line.split(':',1)[0]: int(line.split()[1])*1024 for line in Path('/proc/meminfo').read_text().splitlines()}
used=memory['MemTotal']-memory['MemAvailable']
rx=tx=0
for line in Path('/proc/net/dev').read_text().splitlines()[2:]:
    name, values=line.split(':',1)
    if name.strip() == 'lo': continue
    fields=values.split()
    rx += int(fields[0]); tx += int(fields[8])
print(cpu,used,rx,tx)
PY`))
	if len(values) != 4 {
		p.t.Fatal("guest resource sample is incomplete")
	}
	parsed := make([]uint64, len(values))
	for index, value := range values {
		var err error
		parsed[index], err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			p.t.Fatal("guest resource sample is invalid")
		}
	}
	return livePersistenceResourceSample{CPUUsec: parsed[0], MemoryUsed: parsed[1], RXBytes: parsed[2], TXBytes: parsed[3]}
}

func (p *livePersistenceProof) prepareUsefulState() {
	p.exec(`set -eu
cd /workspace
git init -q
git config user.name "Synthetic Proof"
git config user.email "proof@example.invalid"
printf 'committed\n' > tracked.txt
git add tracked.txt
git commit -qm baseline
printf 'working-tree-edit\n' >> tracked.txt
printf 'untracked\n' > untracked.txt
mkdir -p .dorf-proof
printf 'Persistent synthetic instruction.\n' > /root/.codex/AGENTS.md
cat >> /root/.codex/config.toml <<'CONFIG'
[mcp_servers.backup_proof]
command = "/bin/false"
enabled = false
[mcp_servers.backup_proof.env]
SYNTHETIC_TOKEN = "confidential-fixture"
CONFIG
printf '{"OPENAI_API_KEY":"synthetic-backup-key"}\n' > /root/.codex/auth.json
printf '{"synthetic":"file-based-mcp-credential"}\n' > /root/.codex/.credentials.json
printf 'client-owned state\n' > /root/.codex/client-state.txt
mkdir -p /root/.codex/log /root/.codex/shell_snapshots
printf 'transient\n' > /root/.codex/log/backup-excluded
printf 'transient\n' > /root/.codex/shell_snapshots/backup-excluded
sha256sum /root/.codex/config.toml /root/.codex/auth.json /root/.codex/.credentials.json /root/.codex/client-state.txt > .dorf-proof/native-files.sha256
chmod 600 /root/.codex/auth.json /root/.codex/.credentials.json
cat > .dorf-proof/hold-wal.py <<'PY'
import sqlite3, time
db=sqlite3.connect('/workspace/.dorf-proof/useful.sqlite')
db.execute('pragma journal_mode=wal')
db.execute('pragma wal_autocheckpoint=0')
db.execute('create table evidence(value text)')
db.execute("insert into evidence values ('retained-wal-row')")
db.commit()
open('/workspace/.dorf-proof/wal-ready','w').write('ready\n')
time.sleep(600)
PY
nohup python3 .dorf-proof/hold-wal.py </dev/null >/tmp/dorf-persistence-wal.log 2>&1 &
for i in $(seq 1 100); do test -s .dorf-proof/useful.sqlite-wal && test -f .dorf-proof/wal-ready && exit 0; sleep .05; done
exit 1`)
}

func (p *livePersistenceProof) preflightNativeCapture(additionalPaths []string) {
	p.t.Helper()
	session, err := p.store.Session(p.ctx, p.session.ID)
	if err != nil {
		p.t.Fatal(err)
	}
	owned, err := p.store.Sandbox(p.ctx, p.sandboxID)
	if err != nil {
		p.t.Fatal(err)
	}
	guard, err := p.agent.BeginPersistenceCapture(p.ctx, livePersistenceOwner(owned), livePersistenceWorkspace, session.ThreadID, additionalPaths)
	if err != nil {
		p.t.Fatalf("native capture preflight setup: %v", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 5*time.Second)
		defer cancel()
		if err := p.agent.CancelPersistenceCapture(cleanupCtx, livePersistenceOwner(owned), guard); err != nil {
			p.t.Errorf("clean native capture preflight: %v", err)
		}
	}()
	select {
	case <-p.ctx.Done():
		p.t.Fatal(p.ctx.Err())
	case <-time.After(2 * time.Second):
	}
	if err := p.agent.FinishPersistenceCapture(p.ctx, livePersistenceOwner(owned), guard); err != nil {
		p.t.Fatalf("finish native capture preflight: %v", err)
	}
}

func (p *livePersistenceProof) createSlowCaptureInput(index int) {
	p.exec("dd if=/dev/urandom of=/workspace/.dorf-proof/cancel-input-$1.bin bs=1M count=192 status=none", strconv.Itoa(index))
}

func (p *livePersistenceProof) removeSlowCaptureInput(index int) {
	p.exec("rm -f /workspace/.dorf-proof/cancel-input-$1.bin", strconv.Itoa(index))
}

func (p *livePersistenceProof) waitIdle() {
	p.t.Helper()
	for {
		boundary, err := p.store.Boundary(p.ctx, p.sandboxID, false)
		if err != nil {
			p.t.Fatal(err)
		}
		if boundary.Eligible && time.Since(boundary.LastActivityAt) >= 5*time.Second {
			return
		}
		select {
		case <-p.ctx.Done():
			p.t.Fatal(p.ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (p *livePersistenceProof) checkpointService(cfg config.Config) persistence.Service {
	p.t.Helper()
	boundary, err := p.store.Boundary(p.ctx, p.sandboxID, false)
	if err != nil {
		p.t.Fatal(err)
	}
	resolver := profileRuntimeResolver{cfg: cfg, store: p.store, client: p.tasks, emit: p.emit}
	service, err := resolver.checkpointService(p.ctx, boundary)
	if err != nil {
		p.t.Fatal(err)
	}
	service.Claim = func(context.Context) error { return nil }
	return service
}

func (p *livePersistenceProof) waitForResticBackup(captureDone <-chan error) {
	p.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-captureDone:
			p.t.Fatalf("checkpoint ended before restic backup became observable: %v", err)
		default:
		}
		if p.execLabeled("observe restic backup", `python3 - <<'PY'
from pathlib import Path
for process in Path('/proc').glob('[0-9]*'):
    try: command=(process/'cmdline').read_bytes().split(b'\0')
    except (FileNotFoundError, PermissionError, ProcessLookupError): continue
    if any(part.endswith(b'/restic') or part == b'restic' for part in command) and b'backup' in command:
        print('running'); break
PY`) == "running" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case err := <-captureDone:
		p.t.Fatalf("checkpoint ended before restic backup became observable: %v", err)
	default:
	}
	p.t.Fatal("restic backup did not become observable before the cancellation window closed")
}

func (p *livePersistenceProof) emit(event telemetry.Event) {
	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()
	p.events = append(p.events, event)
}

func (p *livePersistenceProof) requireCancellationEvidence(want int) {
	p.t.Helper()
	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()
	count := 0
	for _, event := range p.events {
		if event.Name == "dorf.checkpoint.storage" && event.Attributes["dorf.cancelled"] == true && event.Attributes["dorf.remote_stopped"] == true {
			count++
		}
	}
	if count != want {
		p.t.Fatalf("checkpoint telemetry confirmed %d cancellation stops, want %d", count, want)
	}
}

func (p *livePersistenceProof) completedLogicalDataAdded() int64 {
	p.eventsMu.Lock()
	defer p.eventsMu.Unlock()
	for index := len(p.events) - 1; index >= 0; index-- {
		if p.events[index].Name == "dorf.checkpoint.storage" && p.events[index].Attributes["dorf.cancelled"] == false {
			if value, ok := p.events[index].Attributes["dorf.data_added_logical_bytes"].(uint64); ok {
				return int64(value)
			}
		}
	}
	return 0
}

func (p *livePersistenceProof) verifyGuestAllocation() {
	p.t.Helper()
	values := strings.Fields(p.execLabeled("inspect guest allocation", `set -eu
cpus=$(nproc)
memory_kib=$(awk '$1 == "MemTotal:" {print $2}' /proc/meminfo)
test -n "$cpus" && test -n "$memory_kib"
printf '%s %s\n' "$cpus" "$memory_kib"`))
	if len(values) != 2 {
		p.t.Fatal("guest did not expose its hypervisor CPU and memory allocation")
	}
	cpus, _ := strconv.ParseInt(values[0], 10, 64)
	memoryKiB, _ := strconv.ParseInt(values[1], 10, 64)
	if cpus <= 0 || cpus > 4 || memoryKiB <= 0 || memoryKiB > 4<<20 {
		p.t.Fatalf("guest exceeds 4 vCPU or 4 GiB proof bound")
	}
	p.execLabeled("verify pinned guest tools", `test "$(codex --version)" = "codex-cli 0.154.0"
test -x /nix/var/nix/profiles/dorf-tools/bin/restic
test "$(/nix/var/nix/profiles/dorf-tools/bin/restic version | awk '{print $1}')" = restic`)
}

func (p *livePersistenceProof) verifyRestoredUsefulState() {
	p.exec(`set -eu
cd /workspace
git fsck --no-dangling >/dev/null
test "$(cat tracked.txt)" = "committed
working-tree-edit"
test "$(cat untracked.txt)" = untracked
git status --porcelain | grep -F " M tracked.txt" >/dev/null
git status --porcelain | grep -F "?? untracked.txt" >/dev/null
test "$(cat /root/.codex/AGENTS.md)" = "Persistent synthetic instruction."
sha256sum --check --status .dorf-proof/native-files.sha256
test ! -e /root/.codex/log/backup-excluded
test ! -e /root/.codex/shell_snapshots/backup-excluded
python3 - <<'CONFIG'
import json, subprocess
server=subprocess.Popen(['codex','app-server'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True)
def call(id,method,params):
    server.stdin.write(json.dumps({'id':id,'method':method,'params':params})+'\n')
    server.stdin.flush()
    for line in server.stdout:
        response=json.loads(line)
        if response.get('id')==id:
            assert 'error' not in response, 'native config request rejected'
            return response['result']
    raise RuntimeError('native config server closed')
try:
    call(1,'initialize',{'clientInfo':{'name':'backup-proof','version':'1'}})
    config=call(2,'config/read',{'includeLayers':False})['config']
    mcp=config['mcp_servers']['backup_proof']
    assert mcp['command']=='/bin/false' and mcp['enabled']==False
    assert mcp['env']['SYNTHETIC_TOKEN']=='confidential-fixture'
finally:
    server.terminate()
    server.wait(timeout=5)
CONFIG
python3 - <<'PY'
import sqlite3
db=sqlite3.connect('/workspace/.dorf-proof/useful.sqlite')
assert db.execute('pragma integrity_check').fetchone()[0]=='ok'
assert db.execute('select value from evidence').fetchone()[0]=='retained-wal-row'
PY`)
}

func (p *livePersistenceProof) requireRestoredConversation(marker string) {
	p.t.Helper()
	contents := p.exec("cat /workspace/.dorf-proof/requests.jsonl")
	if !strings.Contains(contents, marker) || len(strings.Split(strings.TrimSpace(contents), "\n")) < 4 {
		p.t.Fatal("restored native request did not contain the original conversation context")
	}
}

func (p *livePersistenceProof) registerCleanup() {
	p.t.Helper()
	p.t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		resources, err := p.store.SandboxResources(cleanupCtx, p.session.ID)
		if err != nil {
			p.t.Errorf("load live proof resources for cleanup: %v", err)
			return
		}
		for _, resource := range resources {
			owner := provider.Ownership{SessionID: p.session.ID, SandboxID: p.sandboxID, OwnershipNonce: resource.OwnershipNonce}
			if err := p.sandbox.DeleteOwned(cleanupCtx, owner); err != nil {
				p.t.Errorf("delete live proof resource: %v", err)
				continue
			}
			present, err := p.sandbox.OwnedPresent(cleanupCtx, owner)
			if err != nil || present {
				p.t.Errorf("confirm live proof resource deletion: present=%t err=%v", present, err)
			}
		}
	})
}

func (p *livePersistenceProof) registerEventExport() {
	p.t.Helper()
	path := os.Getenv("DORF_LIVE_CHECKPOINT_EVENTS")
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		p.t.Fatal("DORF_LIVE_CHECKPOINT_EVENTS must be one absolute private output path")
	}
	p.t.Cleanup(func() {
		p.eventsMu.Lock()
		events := append([]telemetry.Event(nil), p.events...)
		p.eventsMu.Unlock()
		output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			p.t.Errorf("create private checkpoint event evidence: %v", err)
			return
		}
		encoder := json.NewEncoder(output)
		for _, event := range events {
			if err := encoder.Encode(event); err != nil {
				_ = output.Close()
				p.t.Errorf("write private checkpoint event evidence: %v", err)
				return
			}
		}
		if err := output.Close(); err != nil {
			p.t.Errorf("close private checkpoint event evidence: %v", err)
			return
		}
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			p.t.Errorf("checkpoint event evidence is not a private regular file")
		}
	})
}

func livePersistenceOwner(owned core.Sandbox) provider.Ownership {
	return provider.Ownership{SessionID: owned.SessionID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce}
}

func installLivePersistenceFixture(ctx context.Context, sandbox provider.Sandbox, owner provider.Ownership, fixture []byte, routeKey string) error {
	if err := sandbox.PutFile(ctx, owner, livePersistenceWorkspace+"/.dorf-proof/responses-fixture.py", fixture); err != nil {
		return err
	}
	if err := sandbox.PutFile(ctx, owner, "/root/.config/dorf/provider-route.key", []byte(routeKey+"\n")); err != nil {
		return err
	}
	result, err := sandbox.Exec(ctx, owner, nil, "bash", "-c", `set -eu
mkdir -p /workspace/.dorf-proof /root/.codex
if ! test -e /root/.codex/config.toml; then
  cat > /root/.codex/config.toml <<'CONFIG'
model_provider = "proof"
[model_providers.proof]
name = "proof"
base_url = "http://127.0.0.1:18997/v1"
wire_api = "responses"
CONFIG
fi
if ! curl -fsS http://127.0.0.1:18997/ >/dev/null; then
  DORF_RESPONSES_FIXTURE_ROOT=/workspace/.dorf-proof nohup python3 /workspace/.dorf-proof/responses-fixture.py </dev/null >/tmp/dorf-persistence-model.log 2>&1 &
fi
for i in $(seq 1 400); do curl -fsS http://127.0.0.1:18997/ >/dev/null && exit 0; sleep .05; done
exit 1`)
	if err != nil {
		return fmt.Errorf("start local Responses fixture: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("start local Responses fixture exited %d", result.ExitCode)
	}
	return nil
}

var errLivePersistenceLostVerificationReceipt = errors.New("injected lost verification receipt")
