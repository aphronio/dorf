package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	profileapp "github.com/aphronio/dorf/internal/profile"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestPostgresCodebaseInvestigationIdentityAndFollowUps(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("codebase-investigation-%d", time.Now().UnixNano())
	input := investigation.Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: key, Workflow: investigation.Workflow, WorkflowRevision: investigation.WorkflowRevision,
			Goal: "Find one unnecessary coding-workflow dependency.", SandboxProfile: "incus",
			ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		},
		Source: investigation.Source{Kind: investigation.SourceRemote, Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40)},
	}
	job, created, err := store.AdmitInvestigation(ctx, input)
	if err != nil || !created || job.Workflow != input.Workflow || job.WorkflowRevision != input.WorkflowRevision {
		t.Fatalf("Job=%#v created=%v err=%v", job, created, err)
	}
	repeated, created, err := store.AdmitInvestigation(ctx, input)
	if err != nil || created || repeated.ID != job.ID {
		t.Fatalf("idempotent Job=%#v created=%v err=%v", repeated, created, err)
	}
	source, err := store.CodebaseInvestigationSource(ctx, job.ID)
	if err != nil || source.JobID != job.ID || source.Kind != investigation.SourceRemote || source.Repository != input.Source.Repository || source.Revision != input.Source.Revision {
		t.Fatalf("source=%#v err=%v", source, err)
	}
	changedSource := input
	changedSource.Source = investigation.Source{
		Kind: investigation.SourceGitBundle, Revision: input.Source.Revision,
		BundleDigest: strings.Repeat("e", 64), BundleByteSize: 123,
	}
	if _, _, err := store.AdmitInvestigation(ctx, changedSource); err == nil || !strings.Contains(err.Error(), "different complete Job input") {
		t.Fatalf("same admission key changed source identity: %v", err)
	}
	changedWorkflow := codingJobInput(key, input.Goal, input.Source.Revision, "dorf/cross-workflow")
	if _, _, err := store.AdmitCoding(ctx, changedWorkflow); err == nil {
		t.Fatal("same admission key changed workflow identity")
	}
	if _, err := store.DB.ExecContext(ctx, `insert into dorf.coding_to_proposal_inputs(job_id,workflow_name,repository,starting_revision,revision,branch,github_repository,github_installation_id,base_branch) values($1,'coding-to-proposal',$2,$3,$3,$4,$5,$6,$7)`,
		job.ID, "https://github.com/aphronio/dorf.git", input.Source.Revision, "dorf/foreign-workflow", "aphronio/dorf", "42", "main"); err == nil {
		t.Fatal("database attached coding input to an investigation Job")
	}
	if admitted, err := store.AdmitInvestigationMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "too-early", Input: "broaden the question"}); err == nil || admitted.Created {
		t.Fatalf("investigation accepted a follow-up before its first run completed: admitted=%#v err=%v", admitted, err)
	}
	if admitted, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "wrong-workflow", Input: "must not cross workflow authority"}); err == nil || admitted.Created || !strings.Contains(err.Error(), "is not coding-to-proposal") {
		t.Fatalf("coding admission crossed into investigation: admitted=%#v err=%v", admitted, err)
	}

	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].AgentRun.Role != "investigate" || deliveries[0].AgentRun.Capability != "repository-read-report" {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	run := deliveries[0].AgentRun
	if err := store.PrepareAgentRun(ctx, run.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, run.ID, "codex", "thread-investigation", "turn-investigation", "completed"); err != nil {
		t.Fatal(err)
	}
	deliveries, err = store.Deliveries(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	run = deliveries[0].AgentRun
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen {
		t.Fatalf("completed run prematurely closed Job=%#v err=%v", job, err)
	}
	follow, err := store.AdmitInvestigationMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "later", Input: "broaden the question"})
	if err != nil || !follow.Created || follow.Message.Sequence != 2 {
		t.Fatalf("follow-up=%#v err=%v", follow, err)
	}
	deliveries, err = store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 2 || deliveries[1].AgentRun.ThreadID != "thread-investigation" || deliveries[1].AgentRun.Role != "investigate" {
		t.Fatalf("continued deliveries=%#v err=%v", deliveries, err)
	}
	secondRun := deliveries[1].AgentRun
	if err := store.PrepareAgentRun(ctx, secondRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, secondRun.ID, "codex", "thread-investigation", "turn-investigation-2", "completed"); err != nil {
		t.Fatal(err)
	}
	deliveries, err = store.Deliveries(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondRun = deliveries[1].AgentRun
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		t.Fatalf("completed follow-up did not remain available for another follow-up or cleanup: Job=%#v err=%v", job, err)
	}
}

func TestPostgresCodebaseInvestigationRetainsBundleSourceIdentity(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	revision := strings.Repeat("9", 40)
	supplied := investigation.Source{
		Kind: investigation.SourceGitBundle, Revision: revision,
		BundleDigest: strings.Repeat("8", 64), BundleByteSize: 4096,
	}
	job, created, err := store.AdmitInvestigation(ctx, investigation.Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: "bundle-source-" + fmt.Sprint(time.Now().UnixNano()),
			Workflow:     investigation.Workflow, WorkflowRevision: investigation.WorkflowRevision,
			Goal: "Inspect an unpublished commit.", SandboxProfile: "incus",
			ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		},
		Source: supplied,
	})
	if err != nil || !created {
		t.Fatalf("Job=%#v created=%v err=%v", job, created, err)
	}
	stored, err := store.CodebaseInvestigationSource(ctx, job.ID)
	supplied.JobID = job.ID
	if err != nil || stored != supplied {
		t.Fatalf("source=%#v want=%#v err=%v", stored, supplied, err)
	}
	if _, err := store.DB.ExecContext(ctx, `update dorf.codebase_investigation_sources set bundle_digest=null where job_id=$1`, job.ID); err == nil {
		t.Fatal("database accepted incomplete Git-bundle source identity")
	}
}

type investigationExternals struct {
	core.Externals
	gitworkspace.Operations
	mu      sync.Mutex
	turn    core.HarnessTurn
	report  []byte
	submits int
	effects []core.ActionKind
}

type investigationAgentExecution struct {
	store     postgres.Store
	externals *investigationExternals
}

type investigationMessageAdmissions struct{ store postgres.Store }

func (a investigationMessageAdmissions) AdmitAgentMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	return a.store.AdmitInvestigationMessage(ctx, input)
}

func (s investigationAgentExecution) SelectAgentMessage(ctx context.Context, jobID string) (*core.AgentMessageWork, error) {
	return investigation.SelectAgentMessage(ctx, s.store, jobID)
}

func (s investigationAgentExecution) ResolveAgentPrompt(ctx context.Context, execution core.AgentMessageExecution) (string, error) {
	source, err := s.store.CodebaseInvestigationSource(ctx, execution.Job.ID)
	if err != nil {
		return "", err
	}
	return investigation.AgentPrompt(source, execution.Message.Input), nil
}

func (s investigationAgentExecution) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	return investigationAgentOperation{externals: s.externals, execution: execution}, nil
}

type investigationAgentOperation struct {
	externals *investigationExternals
	execution core.AgentMessageExecution
}

func (o investigationAgentOperation) Harness() string { return "codex" }
func (o investigationAgentOperation) Submit(_ context.Context, run core.AgentRun, _ string) (core.HarnessBinding, error) {
	o.externals.mu.Lock()
	defer o.externals.mu.Unlock()
	if run.ThreadID == "" {
		o.externals.turn = core.HarnessTurn{ID: "turn-" + run.ID, Status: "completed", Output: "Investigation complete."}
		return core.HarnessBinding{Harness: "codex", ThreadID: "thread-" + o.execution.Job.ID, Turn: o.externals.turn}, nil
	}
	if run.ThreadID != "thread-"+o.execution.Job.ID {
		return core.HarnessBinding{}, fmt.Errorf("follow-up did not reuse the investigator Thread")
	}
	o.externals.submits++
	output := "Investigation complete."
	if o.externals.submits == 1 {
		o.externals.report = []byte("# Finding\n\nThe explicit coordinator is in `internal/investigation/coordinator.go`.\n")
	} else {
		o.externals.report = []byte("# Revised finding\n\nThe follow-up is grounded in `internal/investigation/coordinator.go`.\n")
		output = "Investigation revised."
	}
	o.externals.turn = core.HarnessTurn{ID: "turn-" + run.ID, Status: "completed", Output: output}
	return core.HarnessBinding{Harness: "codex", ThreadID: run.ThreadID, Turn: o.externals.turn}, nil
}
func (o investigationAgentOperation) Recover(_ context.Context, _ core.AgentRun) (core.HarnessBinding, error) {
	o.externals.mu.Lock()
	defer o.externals.mu.Unlock()
	if o.externals.turn.ID == "" {
		return core.HarnessBinding{}, nil
	}
	return core.HarnessBinding{Harness: "codex", ThreadID: "thread-" + o.execution.Job.ID, Turn: o.externals.turn}, nil
}
func (o investigationAgentOperation) History(_ context.Context, run core.AgentRun) (core.HarnessHistory, error) {
	o.externals.mu.Lock()
	defer o.externals.mu.Unlock()
	threadID := run.ThreadID
	if threadID == "" {
		threadID = "thread-" + o.execution.Job.ID
	}
	turns := []core.HarnessTurn(nil)
	if o.externals.turn.ID != "" {
		turns = append(turns, o.externals.turn)
	}
	return core.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: turns}, nil
}

func (e *investigationExternals) effect(kind core.ActionKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.effects = append(e.effects, kind)
	return nil
}
func (e *investigationExternals) SandboxCreate(context.Context, core.Job, core.Sandbox) error {
	return e.effect(core.ActionSandboxCreate)
}
func (e *investigationExternals) ReconcileClone(context.Context, provider.Ownership, string, string, string) error {
	return e.effect(gitworkspace.ActionRepositoryClone)
}
func (e *investigationExternals) Reconcile(_ context.Context, job core.Job, _ core.Sandbox, source investigation.Source, contents []byte) error {
	if source.JobID != job.ID || string(contents) != "retained repository input" {
		return fmt.Errorf("unexpected retained repository restore")
	}
	return e.effect(investigation.ActionRepositoryRestore)
}
func (e *investigationExternals) RouteCreate(context.Context, core.Job, core.Sandbox, core.Route) error {
	return e.effect(core.ActionRouteCreate)
}
func (e *investigationExternals) RouteRevoke(context.Context, core.Job, core.Sandbox, core.Route) error {
	return e.effect(core.ActionRouteRevoke)
}
func (e *investigationExternals) SandboxDelete(context.Context, core.Job, core.Sandbox) error {
	return e.effect(core.ActionSandboxDelete)
}
func (e *investigationExternals) SteerHistory(_ context.Context, _ core.Job, _ string, threadID string) (core.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return core.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: []core.HarnessTurn{e.turn}}, nil
}
func (*investigationExternals) ObserveRevision(_ context.Context, _ provider.Ownership, _ string, revision string) (gitworkspace.Observation, error) {
	now := time.Now().UTC()
	return gitworkspace.Observation{ComparisonBase: revision, Revision: revision, Tree: strings.Repeat("c", 40), StartedAt: now, FinishedAt: now}, nil
}
func (e *investigationExternals) ReadSandboxFile(_ context.Context, job core.Job, sandbox core.Sandbox, relativePath string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if sandbox.JobID != job.ID || relativePath != investigation.ReportPath {
		return nil, fmt.Errorf("workspace file %q: %w", relativePath, os.ErrNotExist)
	}
	if e.report == nil {
		return nil, fmt.Errorf("workspace file %q: %w", relativePath, os.ErrNotExist)
	}
	return append([]byte(nil), e.report...), nil
}

func TestPostgresCodebaseInvestigationResumesOneOpenIdleTaskAfterRestart(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	records := blob.Store{Root: t.TempDir()}
	retained, err := records.Put([]byte("retained repository input"))
	if err != nil {
		t.Fatal(err)
	}
	externals := &investigationExternals{}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(investigationAgentExecution{store: store, externals: externals})
	workspaceExecutor := gitworkspace.NewExecutor(execution, externals, nil)
	service := investigation.NewService(workspaceExecutor, externals, records)
	runtimeProfile := profileapp.Runtime{SandboxProfile: "incus"}
	resolver := integrationRuntimeResolver{
		execution: execution, files: externals, profile: runtimeProfile,
		investigationRuntime: investigation.Runtime{Profile: runtimeProfile, Agent: execution, Investigation: service},
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	client := newFaultClient(t, store, "dorf_investigation_idle_"+suffix)
	application := core.Application{
		Store: store, Tasks: client, AgentMessages: investigationMessageAdmissions{store: store},
		SandboxRuntimes: resolver, CleanupRuntimes: resolver,
	}
	application.RegisterCleanup()
	investigation.Register(application, store, resolver)
	job, created, err := investigation.Admit(ctx, store, application, providerCheck{}, runtimeProfile, investigation.Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: "investigation-terminal-" + suffix,
			Goal:         "Find one concrete simplification.", SandboxProfile: "incus",
			ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		},
		Source: investigation.Source{
			Kind: investigation.SourceGitBundle, Revision: strings.Repeat("d", 40),
			BundleDigest: retained.Digest, BundleByteSize: retained.ByteSize,
		},
	})
	if err != nil || !created {
		t.Fatalf("Job=%#v created=%v err=%v", job, created, err)
	}
	reportJob, err := application.OpenJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	reportSandbox, err := reportJob.DefaultSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	settleNextRun := func(worker *absurd.Client, workerID string, want int) {
		t.Helper()
		// The first activation submits the Turn and durably suspends for the
		// one-second active observation interval. The second observes the
		// completed Turn and enters the open idle wait without copying report bytes.
		if err := worker.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: workerID, BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond)
		if err := worker.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: workerID, BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
			t.Fatal(err)
		}
		deliveries, err := store.Deliveries(ctx, job.ID)
		if err != nil || len(deliveries) != want || deliveries[want-1].AgentRun.State != core.AgentRunCompleted {
			t.Fatalf("deliveries=%#v want completed count=%d err=%v", deliveries, want, err)
		}
	}
	settleNextRun(client, "investigation-terminal", 1)
	wantEffects := []core.ActionKind{core.ActionSandboxCreate, investigation.ActionRepositoryRestore, core.ActionRouteCreate}
	snapshot, err := investigation.LoadSnapshot(ctx, store, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work := snapshot.Project(); work.Kind != "" || !snapshot.Job.AdmissionOpen || snapshot.Job.CleanupState != core.CleanupPending || snapshot.Job.WorkflowAttention != "" {
		t.Fatalf("completed run without REPORT.md was not honestly open-idle: snapshot=%#v work=%#v", snapshot, work)
	}
	if _, err := reportSandbox.ReadFile(ctx, investigation.ReportPath); !errors.Is(err, os.ErrNotExist) || err.Error() != `workspace file "REPORT.md": file does not exist` {
		t.Fatalf("completed run missing REPORT.md error=%v", err)
	}
	firstReceipt, err := reportSandbox.Agent().Message(ctx, "dogfood-first-report", "Write the complete initial report.")
	if err != nil || !firstReceipt.Created || firstReceipt.Sequence != 2 {
		t.Fatalf("initial-report follow-up receipt=%#v err=%v", firstReceipt, err)
	}
	settleNextRun(client, "investigation-first-report", 2)
	report, err := reportSandbox.ReadFile(ctx, investigation.ReportPath)
	if err != nil || string(report) != "# Finding\n\nThe explicit coordinator is in `internal/investigation/coordinator.go`.\n" {
		t.Fatalf("REPORT.md=%q err=%v", report, err)
	}
	allEvidence, err := store.Evidence(ctx, job.ID)
	if err != nil || len(allEvidence) != 0 {
		t.Fatalf("agent prose was recorded as Evidence: %#v err=%v", allEvidence, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		t.Fatalf("Job did not remain open while it had no current workflow operation: %#v err=%v", job, err)
	}
	attachments, err := store.JobTasks(ctx, job.ID)
	if err != nil || len(attachments) != 1 || attachments[0].TaskID != job.CurrentTaskID || attachments[0].TaskName != investigation.TaskName {
		t.Fatalf("open-idle execution owner=%#v Job=%#v err=%v", attachments, job, err)
	}
	idleTaskID := job.CurrentTaskID
	idleTask, err := client.FetchTaskResult(ctx, client.QueueName(), idleTaskID)
	if err != nil || idleTask == nil || idleTask.State != absurd.TaskSleeping {
		t.Fatalf("open-idle Absurd task=%#v err=%v", idleTask, err)
	}

	// A fresh client with an empty in-memory registry simulates the worker
	// process restarting while the durable task is idle in Absurd.
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := absurd.New(absurd.Options{DB: store.DB, QueueName: client.QueueName()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restartedApplication := core.Application{
		Store: store, Tasks: restarted, AgentMessages: investigationMessageAdmissions{store: store},
		SandboxRuntimes: resolver, CleanupRuntimes: resolver,
	}
	restartedApplication.RegisterCleanup()
	investigation.Register(restartedApplication, store, resolver)
	handle, err := restartedApplication.OpenJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := handle.DefaultSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := sandbox.Agent().Message(ctx, "dogfood-follow-up", "Check whether the recommendation still holds after the recent workflow changes.")
	if err != nil || !receipt.Created || receipt.Sequence != 3 {
		t.Fatalf("follow-up receipt=%#v err=%v", receipt, err)
	}
	settleNextRun(restarted, "investigation-restarted", 3)
	report, err = reportSandbox.ReadFile(ctx, investigation.ReportPath)
	if err != nil || string(report) != "# Revised finding\n\nThe follow-up is grounded in `internal/investigation/coordinator.go`.\n" {
		t.Fatalf("revised REPORT.md=%q err=%v", report, err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 3 || deliveries[0].AgentRun.ThreadID == "" ||
		deliveries[1].AgentRun.ThreadID != deliveries[0].AgentRun.ThreadID || deliveries[2].AgentRun.ThreadID != deliveries[0].AgentRun.ThreadID ||
		deliveries[1].AgentRun.TurnID == deliveries[0].AgentRun.TurnID || deliveries[2].AgentRun.TurnID == deliveries[1].AgentRun.TurnID {
		t.Fatalf("same-Thread resumed deliveries=%#v err=%v", deliveries, err)
	}
	afterFollow, err := store.Job(ctx, job.ID)
	if err != nil || afterFollow.CurrentTaskID != idleTaskID {
		t.Fatalf("follow-up replaced durable execution owner: Job=%#v err=%v", afterFollow, err)
	}
	attachments, err = store.JobTasks(ctx, job.ID)
	if err != nil || len(attachments) != 1 {
		t.Fatalf("follow-up created a duplicate task attachment: %#v err=%v", attachments, err)
	}
	if err := handle.RequestCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	cleaning, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleaning.AdmissionOpen || cleaning.CleanupState != core.CleanupScheduled {
		t.Fatalf("explicit cleanup did not close admission and schedule release: %#v", cleaning)
	}
	if err := restarted.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "investigation-cleanup", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("cleaned Job=%#v err=%v", cleaned, err)
	}
	if _, err := reportSandbox.ReadFile(ctx, investigation.ReportPath); err == nil || !strings.Contains(err.Error(), "unavailable after cleanup begins") {
		t.Fatalf("REPORT.md remained readable after cleanup: %v", err)
	}
	externals.mu.Lock()
	effects := append([]core.ActionKind(nil), externals.effects...)
	externals.mu.Unlock()
	wantEffects = append(wantEffects, core.ActionRouteRevoke, core.ActionSandboxDelete)
	if !slices.Equal(effects, wantEffects) {
		t.Fatalf("effects after cleanup=%v want=%v", effects, wantEffects)
	}
}
