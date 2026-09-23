package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/persistence"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/terminal"
)

// TestLiveCheckpointBranch exercises the operator command and the real direct
// and cleanup tasks against two concurrent E2B VMs and R2. Model Turns use a
// synthetic Responses fixture; Gateway route issuance uses a local control
// fixture. It is a bounded provider lifecycle proof, not a real model eval.
func TestLiveCheckpointBranch(t *testing.T) {
	if os.Getenv("DORF_LIVE_BRANCH_E2B") != "1" {
		t.Skip("set DORF_LIVE_BRANCH_E2B=1 with a private two-VM budget reservation")
	}
	budget := readLivePersistenceBudget(t)
	manifest := readLivePersistenceManifest(t)
	baseConfig := readLivePersistenceConfig(t)
	fixture, err := os.ReadFile("../../scripts/runtime-upgrade/responses-fixture.py")
	if err != nil {
		t.Fatal(err)
	}
	db, store, tasks := livePersistenceDatabase(t)
	t.Cleanup(func() { tasks.Close(); db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	profile := livePersistenceProfile(t, ctx, store, budget.RunID, manifest.Template.Reference)
	cfg := config.Config{PersistenceFile: writeLivePersistenceConfig(t, baseConfig, profile.Ref()),
		Workspace: livePersistenceWorkspace, AppServerPort: livePersistencePort,
		TurnTimeout: 2 * time.Minute, E2BAPIKey: os.Getenv("E2B_API_KEY")}
	sandbox, err := sandboxForProfile(cfg, profile)
	if err != nil {
		t.Fatal(err)
	}
	agent := codex.Agent{Sandbox: sandbox, Port: cfg.AppServerPort, Timeout: cfg.TurnTimeout}
	source, _, err := store.AdmitDirect(ctx, core.SessionAdmission{
		AdmissionKey: "branch-proof-" + budget.RunID, SandboxProfile: profile.Name,
		ProviderConnection: "synthetic-local", Model: "synthetic-model", ReasoningEffort: "low", KeepRunning: true,
	}, tasks.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	sourceSandboxID := core.MainSandboxName(source.ID)
	gateway := newRecoveryGatewayFixture(t)
	owner := func(ctx context.Context, id string) (provider.Ownership, error) {
		owned, err := store.Sandbox(ctx, id)
		return livePersistenceOwner(owned), err
	}
	base := terminal.Externals{Sandbox: sandbox, Agent: agent, Gateway: gateway.gateway(), Ownership: owner}
	externals := liveBranchExternals{Externals: base, sourceSessionID: source.ID, fixture: fixture}
	resolver := profileRuntimeResolver{cfg: cfg, store: store, client: tasks}
	resolved := resolvedBaseRuntime{SandboxProfile: profile.Ref(), Sandbox: sandbox,
		Execution: core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)}
	upgrades, err := resolver.upgradeExecution(ctx, resolved)
	if err != nil {
		t.Fatal(err)
	}
	execution := checkpointExecution{Execution: upgrades, resolver: resolver}
	runtime := livePersistenceRuntime{ref: profile.Ref(), native: base, execution: execution, files: base, commands: base}
	app := core.Application{Store: store, Tasks: tasks, SandboxRuntimes: runtime, CleanupRuntimes: runtime}
	direct.Register(app, store, runtime)
	app.RegisterCleanup()
	reader := controlreader.Service{Store: store, Runtimes: runtime}
	proof := &livePersistenceProof{t: t, ctx: ctx, store: store, tasks: tasks, sandbox: sandbox,
		agent: agent, fixture: fixture, reader: reader, session: source, sandboxID: sourceSandboxID}
	proof.registerCleanup()
	var destinationID, destinationSandboxID string
	t.Cleanup(func() {
		if destinationID == "" {
			return
		}
		cleanupCtx, stop := context.WithTimeout(context.Background(), 90*time.Second)
		defer stop()
		resources, err := store.SandboxResources(cleanupCtx, destinationID)
		if err != nil {
			t.Errorf("load branch resource cleanup: %v", err)
			return
		}
		for _, resource := range resources {
			owned := provider.Ownership{SessionID: destinationID, SandboxID: destinationSandboxID, OwnershipNonce: resource.OwnershipNonce}
			if err := sandbox.DeleteOwned(cleanupCtx, owned); err != nil {
				t.Errorf("delete branch resource: %v", err)
			}
		}
	})
	markLivePersistenceBudgetConsumed(t, budget)
	stopWorker := proof.startWorker()
	initial := proof.admit("branch-source-initial", "Remember the synthetic marker BRANCH_CONTEXT_154. Reply briefly without tools.")
	first, _ := proof.waitNativeTurn(initial)
	if first.Output == "" || initial.ThreadID == "" {
		t.Fatal("source native Turn did not complete")
	}
	proof.verifyGuestAllocation()
	proof.prepareUsefulState()
	proof.exec("printf 'checkpoint-world\\n' > /workspace/.dorf-proof/world.txt")
	proof.waitIdle()
	service := proof.checkpointService(cfg)
	if _, err := service.Capture(ctx, sourceSandboxID, false); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.LastCheckpoint(ctx, sourceSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	proof.exec("printf 'source-later\\n' > /workspace/.dorf-proof/world.txt")
	later := proof.admit("branch-source-later", "Reply briefly with the synthetic marker after the checkpoint. Do not use tools.")
	proof.waitNativeTurn(later)
	requestID := "branch-" + budget.RunID
	var output bytes.Buffer
	if err := checkpointCommand(ctx, store, tasks, cfg,
		[]string{"branch", source.ID, "--id", requestID, "--repository", checkpoint.Repository, "--snapshot", checkpoint.SnapshotID},
		&output, &output); err != nil {
		t.Fatal(err)
	}
	var branch persistence.BranchReceipt
	if err := json.Unmarshal(output.Bytes(), &branch); err != nil {
		t.Fatal(err)
	}
	destinationID, destinationSandboxID = branch.DestinationSessionID, branch.DestinationSandboxID
	if branch.ThreadID != initial.ThreadID || branch.Checkpoint != checkpoint || destinationID == source.ID {
		t.Fatal("branch command did not retain independent exact checkpoint custody")
	}
	branch = waitLiveBranch(t, ctx, store, branch.ID, func(current persistence.BranchReceipt) bool {
		return !current.RestoredAt.IsZero()
	})
	if !branch.ReadyAt.IsZero() {
		t.Fatal("branch started before client preparation")
	}
	stopWorker()
	output.Reset()
	if err := checkpointCommand(ctx, store, tasks, cfg,
		[]string{"branch", source.ID, "--id", requestID, "--repository", checkpoint.Repository, "--snapshot", checkpoint.SnapshotID},
		&output, &output); err != nil {
		t.Fatal(err)
	}
	var replay persistence.BranchReceipt
	if err := json.Unmarshal(output.Bytes(), &replay); err != nil || replay.DestinationSessionID != destinationID || replay.RestoredAt.IsZero() {
		t.Fatal("branch request replay did not retain restored destination")
	}
	destination, err := store.Sandbox(ctx, destinationSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if destination.ProviderID == "" {
		t.Fatal("branch destination was not provisioned")
	}
	branchOwner := livePersistenceOwner(destination)
	branchExec := func(script string) {
		t.Helper()
		result, err := sandbox.Exec(ctx, branchOwner, nil, "bash", "-c", script)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("branch guest assertion failed (exit=%d): %v", result.ExitCode, err)
		}
	}
	branchExec(`set -eu
test "$(cat /workspace/.dorf-proof/world.txt)" = checkpoint-world
test ! -e /root/.config/dorf/provider-route.key
test ! -e /tmp/dorf/codex-app-server.pid
sha256sum --check --status /workspace/.dorf-proof/native-files.sha256`)
	if _, err := reader.ReadNativeTurns(ctx, destinationID); err == nil {
		t.Fatal("held branch exposed native history")
	}
	if _, err := reader.Exec(ctx, destinationSandboxID, provider.Command{Argv: []string{"true"}}); err == nil {
		t.Fatal("held branch allowed arbitrary commands")
	}
	if err := reader.WriteFile(ctx, destinationSandboxID, "/root/.codex/auth.json", []byte(`{"OPENAI_API_KEY":"synthetic-branch-key"}`+"\n"), false); err != nil {
		t.Fatal(err)
	}
	if err := reader.WriteFile(ctx, destinationSandboxID, ".dorf-proof/world.txt", []byte("branch-prepared\n"), false); err != nil {
		t.Fatal(err)
	}
	contents, err := reader.ReadFile(ctx, destinationSandboxID, "/root/.codex/auth.json")
	if err != nil || string(contents) != `{"OPENAI_API_KEY":"synthetic-branch-key"}`+"\n" {
		t.Fatal("held file preparation did not replace native credentials")
	}
	stopWorker = proof.startWorker()
	output.Reset()
	if err := checkpointCommand(ctx, store, tasks, cfg, []string{"release-branch", branch.ID}, &output, &output); err != nil {
		t.Fatal(err)
	}
	branch = waitLiveBranch(t, ctx, store, branch.ID, func(current persistence.BranchReceipt) bool {
		return !current.ReadyAt.IsZero()
	})
	branchExec(`set -eu
test "$(cat /workspace/.dorf-proof/world.txt)" = branch-prepared
test -s /root/.config/dorf/provider-route.key
test -s /tmp/dorf/codex-app-server.pid`)
	if err := gateway.assertFreshRoute(1); err != nil {
		t.Fatal(err)
	}
	// The Gateway control fixture proves route custody but does not serve model
	// inference. Switch only this ready destination to the local Responses fixture.
	if err := agent.Quiesce(ctx, branchOwner, branch.ThreadID); err != nil {
		t.Fatal(err)
	}
	if err := gateway.revoke(ctx, destination); err != nil {
		t.Fatal(err)
	}
	if err := agent.RemoveRoute(ctx, branchOwner); err != nil {
		t.Fatal(err)
	}
	if err := installLivePersistenceFixture(ctx, sandbox, branchOwner, fixture, "synthetic-branch-route"); err != nil {
		t.Fatal(err)
	}
	continued, err := reader.SubmitEvent(ctx, destinationID, core.NativeEvent{Type: core.InputMessage,
		ClientID: "branch-native-continuation", Text: "Reply with the synthetic marker remembered before branching. Do not use tools."})
	if err != nil || continued.ThreadID != initial.ThreadID {
		t.Fatalf("branch did not accept original native Thread: %v", err)
	}
	end := waitLiveNativeTurn(t, ctx, reader, destinationID, continued)
	if strings.TrimSpace(end.Output) == "" {
		t.Fatal("branch native continuation omitted output")
	}
	branchExec(`set -eu
test "$(cat /workspace/.dorf-proof/world.txt)" = branch-prepared
grep -Fq BRANCH_CONTEXT_154 /workspace/.dorf-proof/requests.jsonl`)
	if proof.exec("cat /workspace/.dorf-proof/world.txt") != "source-later" {
		t.Fatal("branch mutation reached source workspace")
	}
	for {
		boundary, err := store.Boundary(ctx, destinationSandboxID, false)
		if err != nil {
			t.Fatal(err)
		}
		if boundary.Eligible && time.Since(boundary.LastActivityAt) >= 5*time.Second {
			break
		}
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The destination has its own logical repository even though the configured
	// R2 bucket and repository ID are shared.
	boundary, err := store.Boundary(ctx, destinationSandboxID, false)
	if err != nil {
		t.Fatal(err)
	}
	destinationService, err := resolver.checkpointService(ctx, boundary)
	if err != nil {
		t.Fatal(err)
	}
	destinationService.Claim = func(context.Context) error { return nil }
	if _, err := destinationService.Capture(ctx, destinationSandboxID, false); err != nil {
		t.Fatal(err)
	}
	destinationCheckpoint, err := store.LastCheckpoint(ctx, destinationSandboxID)
	if err != nil || destinationCheckpoint.SessionID != destinationID || destinationCheckpoint.SandboxID != destinationSandboxID ||
		destinationCheckpoint.NativeRevision <= checkpoint.NativeRevision {
		t.Fatalf("destination checkpoint did not have independent custody: %v", err)
	}
	if err := store.ScheduleCleanup(ctx, tasks.QueueName(), destinationID, ""); err != nil {
		t.Fatal(err)
	}
	for {
		cleaned, err := store.Session(ctx, destinationID)
		if err != nil {
			t.Fatal(err)
		}
		if cleaned.CleanupState == core.CleanupComplete {
			break
		}
		if cleaned.CleanupAttention != "" {
			t.Fatalf("branch cleanup needs attention: %s", cleaned.CleanupAttention)
		}
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if present, err := sandbox.OwnedPresent(ctx, branchOwner); err != nil || present {
		t.Fatalf("destination resource survived cleanup: present=%t err=%v", present, err)
	}
	if held, err := store.SandboxDeliveryHeld(ctx, destinationSandboxID); err != nil || held {
		t.Fatalf("destination hold survived cleanup: held=%t err=%v", held, err)
	}
	sourceAfter, err := store.Session(ctx, source.ID)
	if err != nil || !sourceAfter.AdmissionOpen || sourceAfter.CleanupState != core.CleanupPending {
		t.Fatalf("source Session changed during branch cleanup: %v", err)
	}
	sourceOwner, err := store.Sandbox(ctx, sourceSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if present, err := sandbox.OwnedPresent(ctx, livePersistenceOwner(sourceOwner)); err != nil || !present {
		t.Fatalf("source resource disappeared: present=%t err=%v", present, err)
	}
	stopWorker()
	t.Log("checkpoint branch live lifecycle passed: exact R2 restore, held preparation, scoped route, same-Thread synthetic continuation, independent checkpoint, destination-only cleanup")
}

type liveBranchExternals struct {
	terminal.Externals
	sourceSessionID string
	fixture         []byte
}

func (e liveBranchExternals) RouteCreate(ctx context.Context, session core.Session, owned core.Sandbox, route core.Route) error {
	if session.ID == e.sourceSessionID {
		return installLivePersistenceFixture(ctx, e.Sandbox, livePersistenceOwner(owned), e.fixture, "synthetic-source-route")
	}
	return e.Externals.RouteCreate(ctx, session, owned, route)
}

func waitLiveBranch(t *testing.T, ctx context.Context, store interface {
	CheckpointBranch(context.Context, string) (persistence.BranchReceipt, error)
}, id string, ready func(persistence.BranchReceipt) bool) persistence.BranchReceipt {
	t.Helper()
	for {
		receipt, err := store.CheckpointBranch(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if ready(receipt) {
			return receipt
		}
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitLiveNativeTurn(t *testing.T, ctx context.Context, reader controlreader.Service, sessionID string, ack core.NativeAcknowledgement) core.HarnessTurn {
	t.Helper()
	for {
		history, err := reader.ReadNativeTurns(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if history.ThreadID != ack.ThreadID {
			t.Fatal("branch native Thread changed")
		}
		for _, turn := range history.Turns {
			if turn.ID == ack.TurnID && turn.Terminal() {
				if turn.Status != "completed" {
					t.Fatalf("branch native status=%s", turn.Status)
				}
				return turn
			}
		}
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
