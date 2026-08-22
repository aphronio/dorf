package postgres_test

import (
	"context"
	"fmt"
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
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestPostgresCodebaseInvestigationIdentityDraftsAndFollowUps(t *testing.T) {
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
	if _, created, err := store.AdmitInvestigationMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "too-early", Input: "broaden the question"}); err == nil || created {
		t.Fatalf("investigation accepted a follow-up before any draft: created=%v err=%v", created, err)
	}
	if _, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "wrong-workflow", Input: "must not cross workflow authority"}); err == nil || created || !strings.Contains(err.Error(), "is not coding-to-proposal") {
		t.Fatalf("coding admission crossed into investigation: created=%v err=%v", created, err)
	}

	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].AgentRun.Role != "investigate" || deliveries[0].AgentRun.Capability != "repository-read-report" {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	run := deliveries[0].AgentRun
	otherInput := input
	otherInput.AdmissionKey += "-other"
	otherJob, created, err := store.AdmitInvestigation(ctx, otherInput)
	if err != nil || !created {
		t.Fatalf("other Job=%#v created=%v err=%v", otherJob, created, err)
	}
	otherDeliveries, err := store.Deliveries(ctx, otherJob.ID)
	if err != nil || len(otherDeliveries) != 1 {
		t.Fatalf("other deliveries=%#v err=%v", otherDeliveries, err)
	}
	if _, err := store.DB.ExecContext(ctx, `insert into dorf.codebase_investigation_drafts(job_id,agent_run_id,content) values($1,$2,$3)`,
		job.ID, otherDeliveries[0].AgentRun.ID, "foreign draft"); err == nil {
		t.Fatal("investigation Draft accepted an AgentRun owned by another Job")
	}
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
	draftContent := "# First finding\n"
	stored, created, err := store.RecordCodebaseInvestigationDraft(ctx, deliveries[0].Message.ID, draftContent)
	if err != nil || !created || stored.JobID != job.ID || stored.MessageID != deliveries[0].Message.ID || stored.AgentRunID != run.ID || stored.Content != draftContent || !stored.CreatedAt.Equal(run.FinishedAt) {
		t.Fatalf("Draft=%#v created=%v err=%v", stored, created, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen {
		t.Fatalf("draft prematurely closed Job=%#v err=%v", job, err)
	}
	replayed, created, err := store.RecordCodebaseInvestigationDraft(ctx, deliveries[0].Message.ID, draftContent)
	if err != nil || created || replayed != stored {
		t.Fatalf("idempotent Draft=%#v created=%v err=%v", replayed, created, err)
	}
	if _, _, err := store.RecordCodebaseInvestigationDraft(ctx, deliveries[0].Message.ID, "# Changed finding\n"); err == nil || !strings.Contains(err.Error(), "different immutable investigation draft") {
		t.Fatalf("changed Draft replay error=%v", err)
	}
	follow, created, err := store.AdmitInvestigationMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "later", Input: "broaden the question"})
	if err != nil || !created || follow.Sequence != 2 {
		t.Fatalf("follow-up=%#v created=%v err=%v", follow, created, err)
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
	if _, created, err := store.RecordCodebaseInvestigationDraft(ctx, follow.ID, "# Second finding\n"); err != nil || !created {
		t.Fatalf("second draft created=%v err=%v", created, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		t.Fatalf("second draft did not remain available for follow-up or cleanup: Job=%#v err=%v", job, err)
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
	mu      sync.Mutex
	job     core.Job
	turn    core.HarnessTurn
	effects []core.ActionKind
}

type investigationAgentStrategies struct{ store postgres.Store }

func (s investigationAgentStrategies) SelectAgentMessage(ctx context.Context, jobID string) (*core.AgentMessageWork, error) {
	return investigation.SelectAgentMessage(ctx, s.store, jobID)
}

func (s investigationAgentStrategies) ResolveAgentPrompt(ctx context.Context, execution core.AgentMessageExecution) (string, error) {
	source, err := s.store.CodebaseInvestigationSource(ctx, execution.Job.ID)
	if err != nil {
		return "", err
	}
	return investigation.AgentPrompt(source, execution.Message.Input), nil
}

func (investigationAgentStrategies) ResolveAgentHarnessStrategy(context.Context, core.AgentMessageExecution) (core.AgentHarnessStrategy, error) {
	return nil, nil
}

func (*investigationExternals) Harness() string { return "codex" }
func (e *investigationExternals) effect(kind core.ActionKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.effects = append(e.effects, kind)
	return nil
}
func (e *investigationExternals) SandboxCreate(context.Context, core.Job, core.Sandbox) error {
	return e.effect(core.ActionSandboxCreate)
}
func (e *investigationExternals) RepositoryClone(context.Context, core.Job, core.Sandbox, string, string, string) error {
	return e.effect(gitworkspace.ActionRepositoryClone)
}
func (e *investigationExternals) RepositoryRestore(_ context.Context, job core.Job, _ core.Sandbox, source investigation.Source, contents []byte) error {
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
func (e *investigationExternals) AgentInitialTurn(_ context.Context, job core.Job, delivery core.Delivery, _ string) (core.HarnessBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.job = job
	e.turn = core.HarnessTurn{ID: "turn-" + delivery.AgentRun.ID, Status: "completed", Output: "# Finding\n\nThe explicit coordinator is in `internal/investigation/coordinator.go`.\n"}
	return core.HarnessBinding{Harness: "codex", ThreadID: "thread-" + job.ID, Turn: e.turn}, nil
}
func (e *investigationExternals) AgentInitialTurns(context.Context, core.Job, string) (core.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return core.HarnessHistory{Harness: "codex", ThreadID: "thread-" + e.job.ID, Turns: []core.HarnessTurn{e.turn}}, nil
}
func (e *investigationExternals) AgentSubmit(_ context.Context, job core.Job, delivery core.Delivery, _ string) (core.HarnessBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if delivery.AgentRun.ThreadID != "thread-"+job.ID {
		return core.HarnessBinding{}, fmt.Errorf("follow-up did not reuse the investigator Thread")
	}
	e.turn = core.HarnessTurn{ID: "turn-" + delivery.AgentRun.ID, Status: "completed", Output: "# Revised finding\n\nThe follow-up is grounded in `internal/investigation/coordinator.go`.\n"}
	return core.HarnessBinding{Harness: "codex", ThreadID: delivery.AgentRun.ThreadID, Turn: e.turn}, nil
}
func (e *investigationExternals) AgentTurns(_ context.Context, job core.Job, _ string, threadID string) (core.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return core.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: []core.HarnessTurn{e.turn}}, nil
}
func (*investigationExternals) RepositoryRevision(_ context.Context, _ core.Job, _ string, revision string) (gitworkspace.Observation, error) {
	now := time.Now().UTC()
	return gitworkspace.Observation{ComparisonBase: revision, Revision: revision, Tree: strings.Repeat("c", 40), StartedAt: now, FinishedAt: now}, nil
}

func TestPostgresCodebaseInvestigationWaitsForClientCleanupAndRetainsDrafts(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	records := blob.Store{Root: t.TempDir()}
	retained, err := records.Put([]byte("retained repository input"))
	if err != nil {
		t.Fatal(err)
	}
	externals := &investigationExternals{}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentStrategies(investigationAgentStrategies{store: store})
	workspaceExecutor := gitworkspace.NewExecutor(execution, externals)
	service := investigation.NewService(workspaceExecutor, externals, records)
	application := core.Application{
		Store: store, Tasks: client,
		SandboxRuntimes: integrationRuntimeResolver{execution: execution, profile: profileapp.Runtime{SandboxProfile: "incus"}},
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	job, created, err := store.AdmitInvestigation(ctx, investigation.Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: "investigation-terminal-" + suffix,
			Workflow:     investigation.Workflow, WorkflowRevision: investigation.WorkflowRevision,
			Goal: "Find one concrete simplification.", SandboxProfile: "incus",
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
	taskName := "test-codebase-investigation-" + suffix
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		handle, err := application.OpenJob(taskCtx, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		for {
			if err := execution.ReconcileJobAgent(taskCtx, params.JobID); err != nil {
				return core.TaskResultV1{}, err
			}
			work, err := investigation.Run(taskCtx, handle, service, store, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			if work.Kind == investigation.WorkWaitAgent {
				continue
			}
			if work.Kind != investigation.WorkWaitInput {
				return core.TaskResultV1{}, fmt.Errorf("investigation stopped at %s: %s", work.Kind, work.Detail)
			}
			return core.TaskResultV1{JobID: params.JobID, Outcome: "draft-ready"}, nil
		}
	}))
	spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, job.CurrentTaskID, spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), client.QueueName(), spawned.TaskID) })
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "investigation-terminal", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	externals.mu.Lock()
	effects := append([]core.ActionKind(nil), externals.effects...)
	externals.mu.Unlock()
	wantEffects := []core.ActionKind{core.ActionSandboxCreate, investigation.ActionRepositoryRestore, core.ActionRouteCreate}
	if !slices.Equal(effects, wantEffects) {
		t.Fatalf("effects=%v want=%v", effects, wantEffects)
	}
	drafts, err := store.CodebaseInvestigationDrafts(ctx, job.ID)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("Drafts=%#v err=%v", drafts, err)
	}
	if !strings.Contains(drafts[0].Content, "# Finding") {
		t.Fatalf("typed Draft content=%q", drafts[0].Content)
	}
	allEvidence, err := store.Evidence(ctx, job.ID)
	if err != nil || len(allEvidence) != 0 {
		t.Fatalf("agent prose was recorded as Evidence: %#v err=%v", allEvidence, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		t.Fatalf("Job did not remain open for follow-up or cleanup: %#v err=%v", job, err)
	}
	if _, created, err := store.AdmitInvestigationMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "dogfood-follow-up", Input: "Check whether the recommendation still holds after the recent workflow changes."}); err != nil || !created {
		t.Fatalf("follow-up created=%v err=%v", created, err)
	}
	revisionTaskName := taskName + "-follow-up"
	client.MustRegister(absurd.Task(revisionTaskName, func(taskCtx context.Context, params core.JobTaskParams) (core.TaskResultV1, error) {
		handle, err := application.OpenJob(taskCtx, params.JobID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		for {
			if err := execution.ReconcileJobAgent(taskCtx, params.JobID); err != nil {
				return core.TaskResultV1{}, err
			}
			work, err := investigation.Run(taskCtx, handle, service, store, params.JobID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			if work.Kind == investigation.WorkWaitAgent {
				continue
			}
			if work.Kind != investigation.WorkWaitInput {
				return core.TaskResultV1{}, fmt.Errorf("follow-up stopped at %s: %s", work.Kind, work.Detail)
			}
			return core.TaskResultV1{JobID: params.JobID, Outcome: "revised-draft-ready"}, nil
		}
	}))
	revisionTask, err := client.Spawn(ctx, revisionTaskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: revisionTaskName})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, spawned.TaskID, revisionTask.TaskID, revisionTaskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "investigation-follow-up", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	drafts, err = store.CodebaseInvestigationDrafts(ctx, job.ID)
	if err != nil || len(drafts) != 2 {
		t.Fatalf("revised Drafts=%#v err=%v", drafts, err)
	}
	handle, err := application.OpenJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
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
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "investigation-cleanup", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("cleaned Job=%#v err=%v", cleaned, err)
	}
	afterCleanup, err := store.CodebaseInvestigationDrafts(ctx, job.ID)
	if err != nil || len(afterCleanup) != 2 || afterCleanup[0] != drafts[0] {
		t.Fatalf("typed Drafts did not survive cleanup: %#v err=%v", afterCleanup, err)
	}
}
