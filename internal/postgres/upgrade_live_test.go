package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/e2b"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/aphronio/dorf/internal/terminal"
	"github.com/aphronio/dorf/internal/upgrade"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// This opts into real disposable provider resources. The provider gateway is a
// local deterministic Responses fixture; no AI account or user tools are used.
func TestLiveUpgradeRetainedWorker(t *testing.T) {
	selected := os.Getenv("DORF_LIVE_UPGRADE_PROVIDER")
	if selected == "" {
		t.Skip("set DORF_LIVE_UPGRADE_PROVIDER=incus|e2b")
	}
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	sandbox := liveUpgradeSandbox(t, selected)
	checkpoints := sandbox.(provider.Checkpointer)
	id := fmt.Sprintf("worker-upgrade-%s-%d", selected, time.Now().Unix())
	root := filepath.Join("../..", ".dorf/runtime-upgrade", id)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	events, err := os.OpenFile(filepath.Join(root, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	var eventLock sync.Mutex
	emit := func(event telemetry.Event) {
		event.Attributes["dorf.synthetic_proof"] = true
		eventLock.Lock()
		defer eventLock.Unlock()
		_ = json.NewEncoder(events).Encode(event)
	}
	job, _, err := store.AdmitDirect(ctx, core.JobAdmission{AdmissionKey: id, SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-6-astra", ReasoningEffort: "low", KeepRunning: true}, client.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	owned, err := store.Sandbox(ctx, core.MainSandboxName(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	owner := provider.Ownership{JobID: job.ID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce}
	t.Logf("proof=%s job=%s sandbox=%s provider=%s", id, job.ID, owned.ID, selected)
	// A failed proof keeps custody in PostgreSQL for investigation and cleanup.
	t.Logf("retained evidence: %s", root)
	if err := sandbox.ReconcileOwnedCreate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	run := func(command string, args ...string) string {
		t.Helper()
		result, err := sandbox.Exec(ctx, owner, nil, append([]string{"bash", "-c", command, "upgrade-proof"}, args...)...)
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("proof guest command failed (exit=%d): %v", result.ExitCode, err)
		}
		return strings.TrimSpace(result.Stdout)
	}
	run("mkdir -p /opt/dorf-upgrade-proof /workspace/upgrade-worker-proof")
	for _, name := range []string{"guest.sh", "package.nix", "packages.json", "responses-fixture.py"} {
		directory := "../../scripts/sandbox/packages"
		if name == "responses-fixture.py" {
			directory = "../../scripts/runtime-upgrade"
		}
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := sandbox.PutFile(ctx, owner, "/opt/dorf-upgrade-proof/"+name, content); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("stage pinned Nix packages")
	packageDirectory := "/opt/dorf-upgrade-proof"
	if os.Getenv("DORF_UPGRADE_REQUIRE_NIX_IMAGE") == "1" {
		run("test \"$(jq -r .harnesses.codex.package_manager /usr/local/share/dorf/image.json)\" = nix; test \"$(readlink -f /usr/local/bin/codex)\" = \"$(readlink -f /nix/var/nix/profiles/dorf-runner)/bin/codex\"; test ! -e /opt/node/lib/node_modules/@openai/codex; nix --version; pi --version")
		packageDirectory = "/usr/local/share/dorf/packages"
		script, err := os.ReadFile("../../scripts/sandbox/workstation-proof.py")
		if err != nil {
			t.Fatal(err)
		}
		if err := sandbox.PutFile(ctx, owner, "/tmp/dorf-workstation-proof.py", script); err != nil {
			t.Fatal(err)
		}
		result, err := sandbox.Exec(ctx, owner, nil, "browser-python", "/tmp/dorf-workstation-proof.py")
		if err != nil || result.ExitCode != 0 {
			emit(telemetry.Event{Name: "dorf.upgrade.workstation.failed", At: time.Now(), Failed: true, Attributes: map[string]any{"dorf.upgrade_id": id, "dorf.job_id": job.ID, "dorf.provider": selected}})
			t.Fatalf("workstation proof failed (exit=%d): %v\n%s\n%s", result.ExitCode, err, result.Stdout, result.Stderr)
		}
		output := strings.TrimSpace(result.Stdout)
		data := []byte(output[strings.LastIndex(output, "\n")+1:])
		var proof struct {
			Workstation string `json:"workstation"`
			Result      string `json:"result"`
		}
		if err := json.Unmarshal(data, &proof); err != nil || proof.Result != "passed" {
			t.Fatal("workstation proof omitted its verified package identity")
		}
		if err := os.WriteFile(filepath.Join(root, "workstation.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		emit(telemetry.Event{Name: "dorf.upgrade.workstation.verified", At: time.Now(), Attributes: map[string]any{"dorf.upgrade_id": id, "dorf.job_id": job.ID, "dorf.provider": selected, "dorf.workstation_path": proof.Workstation}})
		t.Logf("workstation proof: %s", output)
	} else {
		run("bash /opt/dorf-upgrade-proof/guest.sh bootstrap")
	}
	run("bash \"$1/guest.sh\" stage 0.154.0", packageDirectory)
	run("bash \"$1/guest.sh\" stage 0.147.0", packageDirectory)
	run("bash \"$1/guest.sh\" activate 0.154.0", packageDirectory)
	target := run("readlink -f \"$1/generations/0.154.0\"", packageDirectory)
	oldTarget := run("readlink -f \"$1/generations/0.147.0\"", packageDirectory)
	agent := codex.Agent{Sandbox: sandbox, Port: 8755, Timeout: 2 * time.Minute}
	external := liveUpgradeExternals{Externals: terminal.Externals{Sandbox: sandbox, Agent: agent, Ownership: func(ctx context.Context, id string) (provider.Ownership, error) {
		current, err := store.Sandbox(ctx, id)
		return provider.Ownership{JobID: current.JobID, SandboxID: current.ID, OwnershipNonce: current.OwnershipNonce}, err
	}}}
	driver := liveUpgradeDriver{NativeDriver: upgrade.NativeDriver{Sandbox: sandbox, Checkpointer: checkpoints, Agent: agent, Replace: selected == "e2b"}}
	configure := func(tasks *absurd.Client) {
		base := core.NewExecutionService(store, external, nil, absurdruntime.RequireClaim).WithAgentExecution(liveUpgradeAgent{external.Externals})
		execution := upgrade.Execution{ExecutionService: base, Upgrades: upgrade.Service{Store: store, Driver: driver, Queue: tasks.QueueName(), Provider: selected, Claim: absurdruntime.RequireClaim, Emit: emit}}
		resolver := integrationRuntimeResolver{execution: execution, profile: "incus"}
		app := core.Application{Store: store, Tasks: tasks, SandboxRuntimes: resolver, CleanupRuntimes: resolver}
		direct.Register(app, store, resolver)
		app.RegisterCleanup()
	}
	start := func(tasks *absurd.Client) func() {
		configure(tasks)
		workerCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			done <- tasks.RunWorker(workerCtx, absurd.WorkerOptions{WorkerID: "upgrade-proof", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 2, PollInterval: 20 * time.Millisecond})
		}()
		var once sync.Once
		finish := func() { once.Do(func() { stop(); <-done }) }
		t.Cleanup(finish)
		return finish
	}
	admit := func(key, text string) core.Message {
		result, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: owned.ID, FromKind: core.MessageFromHuman, FromID: key, Input: text, Intent: core.MessageAuto})
		if err != nil {
			t.Fatal(err)
		}
		return result.Message
	}
	waitMessage := func(message core.Message) {
		liveUpgradeWait(t, ctx, func() bool {
			execution, err := store.AgentMessageExecution(ctx, message.ID)
			return err == nil && execution.AgentRun.State == core.AgentRunCompleted
		})
	}
	first := admit("initial", "Remember this synthetic marker: DORF_UPGRADE_WORKER_CONTEXT_732")
	stop := start(client)
	waitMessage(first)
	stop()
	t.Log("original native turn completed; request activation with worker stopped")
	for index, version := range []string{"0.154.0", "0.147.0"} {
		path := target
		if index == 1 {
			path = oldTarget
		}
		request := upgrade.Request{ID: fmt.Sprintf("%s-%d", id, index), JobID: job.ID, SandboxID: owned.ID, PackagePath: path, Version: version}
		if _, err := store.RequestSandboxUpgrade(ctx, client.QueueName(), request); err != nil {
			t.Fatal(err)
		}
		queued := admit(fmt.Sprintf("queued-%d", index), "Continue with the original synthetic context")
		restarted, err := absurd.New(absurd.Options{DB: store.DB, QueueName: client.QueueName()})
		if err != nil {
			t.Fatal(err)
		}
		stop = start(restarted)
		waitMessage(queued)
		stop()
		receipts, err := store.JobUpgrades(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		receipt := receipts[index]
		expected := "upgraded"
		if index == 1 {
			expected = "rolled_back"
		}
		if receipt.Outcome() != expected {
			t.Fatalf("outcome=%q want %q", receipt.Outcome(), expected)
		}
		current, err := store.Sandbox(ctx, owned.ID)
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 && selected == "e2b" && (current.ResourceID == owned.ResourceID || current.ProviderID == receipt.SourceProviderID) {
			t.Fatal("E2B did not switch provider binding")
		}
		data, err := sandbox.ReadFile(ctx, provider.Ownership{JobID: job.ID, SandboxID: current.ID, OwnershipNonce: current.OwnershipNonce}, "/workspace/upgrade-worker-proof/requests.jsonl")
		if err != nil || !strings.Contains(string(data), "DORF_UPGRADE_WORKER_CONTEXT_732") {
			t.Fatal("resumed model request lost original context")
		}
		if count := len(strings.Split(strings.TrimSpace(string(data)), "\n")); count != index+2 {
			t.Fatalf("native model requests=%d want %d", count, index+2)
		}
		execution, err := store.AgentMessageExecution(ctx, queued.ID)
		if err != nil {
			t.Fatal(err)
		}
		history, err := agent.ReadTurns(ctx, provider.Ownership{JobID: job.ID, SandboxID: current.ID, OwnershipNonce: current.OwnershipNonce}, execution.AgentRun.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		substantive := false
		for _, turn := range history.Turns {
			if turn.ID == execution.AgentRun.TurnID && strings.TrimSpace(turn.Output) != "" {
				substantive = true
			}
		}
		if !substantive {
			t.Fatal("queued native Turn completed without a substantive reply")
		}

		t.Logf("verified upgrade=%s outcome=%s provider_before=%s provider_after=%s", request.ID, receipt.Outcome(), receipt.SourceProviderID, current.ProviderID)
	}
	stop = start(client)
	if err := store.ScheduleCleanup(ctx, client.QueueName(), job.ID, ""); err != nil {
		t.Fatal(err)
	}
	liveUpgradeWait(t, ctx, func() bool {
		current, err := store.Job(ctx, job.ID)
		return err == nil && current.CleanupState == core.CleanupComplete
	})
	stop()
	t.Log("verified all resource and checkpoint cleanup")
}

func liveUpgradeWait(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("retained worker did not converge; inspect saved Job and upgrade receipts")
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func liveUpgradeSandbox(t *testing.T, selected string) provider.Sandbox {
	t.Helper()
	if selected == "incus" {
		var manifest struct {
			Image string `json:"image_fingerprint"`
		}
		manifestPath := os.Getenv("DORF_UPGRADE_INCUS_MANIFEST")
		if manifestPath == "" {
			manifestPath = "../../dist/release-0.10.1/dorf-incus-vm-v5-x86_64.json"
		}
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		connection := incus.DefaultConnectionConfig()
		connection.Project = "dorf-runtime-upgrade-proof"
		if project := os.Getenv("DORF_UPGRADE_INCUS_PROJECT"); project != "" {
			connection.Project = project
		}
		return incus.Adapter{Sandbox: incus.Sandbox{Config: incus.Config{Image: manifest.Image, Network: "incusbr0", DiskSize: "50GiB", Workspace: "/workspace", Connection: connection}}}
	}
	if selected != "e2b" {
		t.Fatal("unknown live upgrade provider")
	}
	var manifest struct {
		Template struct {
			Reference string `json:"reference"`
		} `json:"template"`
	}
	manifestPath := os.Getenv("DORF_E2B_PROFILE_MANIFEST")
	if manifestPath == "" {
		manifestPath = "../../dist/e2b-template/profile.json"
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("E2B_API_KEY") == "" {
		t.Fatal("E2B_API_KEY is not configured")
	}
	return e2b.Adapter{Client: e2b.Client{APIKey: os.Getenv("E2B_API_KEY")}, Config: e2b.AdapterConfig{Template: manifest.Template.Reference, Workspace: "/workspace", SandboxTimeout: time.Hour, ProcessTimeout: 10 * time.Minute, AllowInternet: true}}
}

type liveUpgradeExternals struct{ terminal.Externals }

func (e liveUpgradeExternals) RouteCreate(ctx context.Context, _ core.Job, s core.Sandbox, _ core.Route) error {
	owner := provider.Ownership{JobID: s.JobID, SandboxID: s.ID, OwnershipNonce: s.OwnershipNonce}
	if err := e.Sandbox.PutFile(ctx, owner, "/root/.codex/config.toml", []byte("model_provider = \"proof\"\n[model_providers.proof]\nname = \"proof\"\nbase_url = \"http://127.0.0.1:18997/v1\"\nwire_api = \"responses\"\n")); err != nil {
		return err
	}
	if err := e.Sandbox.PutFile(ctx, owner, "/root/.config/dorf/provider-route.key", []byte("synthetic-fixture-only\n")); err != nil {
		return err
	}
	return liveUpgradeFixture(ctx, e.Sandbox, owner)
}
func (e liveUpgradeExternals) RouteRevoke(ctx context.Context, _ core.Job, s core.Sandbox, _ core.Route) error {
	return e.Agent.RemoveRoute(ctx, provider.Ownership{JobID: s.JobID, SandboxID: s.ID, OwnershipNonce: s.OwnershipNonce})
}

type liveUpgradeAgent struct{ terminal.Externals }

func (e liveUpgradeAgent) ResolveAgentPrompt(_ context.Context, execution core.AgentMessageExecution) (string, error) {
	return execution.Message.Input, nil
}
func (e liveUpgradeAgent) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	return terminal.NewAgentRunOperation(e.Externals, execution)
}

type liveUpgradeDriver struct{ upgrade.NativeDriver }

func (d liveUpgradeDriver) Verify(ctx context.Context, s core.Sandbox, version string, runs []core.AgentRun) error {
	if err := liveUpgradeFixture(ctx, d.Sandbox, provider.Ownership{JobID: s.JobID, SandboxID: s.ID, OwnershipNonce: s.OwnershipNonce}); err != nil {
		return err
	}
	if err := d.NativeDriver.Verify(ctx, s, version, runs); err != nil {
		return err
	}
	owner := provider.Ownership{JobID: s.JobID, SandboxID: s.ID, OwnershipNonce: s.OwnershipNonce}
	if version == "0.147.0" {
		if err := d.Sandbox.PutFile(ctx, owner, "/root/.codex/upgrade-incompatible-state", []byte("injected migrated state")); err != nil {
			return err
		}
		return fmt.Errorf("injected verification failure after local-state mutation")
	}
	result, err := d.Sandbox.Exec(ctx, owner, nil, "test", "!", "-e", "/root/.codex/upgrade-incompatible-state")
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("rollback retained incompatible runner state")
	}
	return nil
}
func liveUpgradeFixture(ctx context.Context, sandbox provider.Sandbox, owner provider.Ownership) error {
	result, err := sandbox.Exec(ctx, owner, nil, "bash", "-c", `if ! curl -fsS http://127.0.0.1:18997/ >/dev/null; then nohup python3 /opt/dorf-upgrade-proof/responses-fixture.py </dev/null >/tmp/dorf-upgrade-model.log 2>&1 & fi; for i in $(seq 1 50); do curl -fsS http://127.0.0.1:18997/ >/dev/null && exit 0; sleep .1; done; exit 1`)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("local model fixture failed")
	}
	return nil
}

var _ upgrade.Store = postgres.Store{}
