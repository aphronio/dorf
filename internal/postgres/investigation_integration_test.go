package postgres_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlplane"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/repository"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestPostgresCodebaseInvestigationIdentityDraftsAndFollowUps(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("codebase-investigation-%d", time.Now().UnixNano())
	input := postgres.NewInvestigationJob{
		NewJob: postgres.NewJob{
			AdmissionKey: key, Workflow: spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision,
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
	changed := input
	changed.Workflow = spine.WorkflowCodingToProposal
	changed.WorkflowRevision = spine.CodingToProposalRevision
	if _, _, err := store.AdmitInvestigation(ctx, changed); err == nil {
		t.Fatal("same admission key changed workflow identity")
	}
	if _, err := store.DB.ExecContext(ctx, `insert into dorf.coding_to_proposal_inputs(job_id,workflow_name,repository,starting_revision,revision,branch,github_repository,github_installation_id,base_branch) values($1,'coding-to-proposal',$2,$3,$3,$4,$5,$6,$7)`,
		job.ID, "https://github.com/aphronio/dorf.git", input.Source.Revision, "dorf/foreign-workflow", "aphronio/dorf", "42", "main"); err == nil {
		t.Fatal("database attached coding input to an investigation Job")
	}
	if _, created, err := store.AdmitInvestigationMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "too-early", Input: "broaden the question"}); err == nil || created {
		t.Fatalf("investigation accepted a follow-up before any draft: created=%v err=%v", created, err)
	}
	if _, created, err := store.AdmitCodingMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "wrong-workflow", Input: "must not cross workflow authority"}); err == nil || created || !strings.Contains(err.Error(), "is not coding-to-proposal") {
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
	if _, err := store.DB.ExecContext(ctx, `insert into dorf.artifacts(id,job_id,name,digest,byte_size,media_type,producer,agent_run_id,created_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		"artifact-cross-job", job.ID, "cross-job.txt", strings.Repeat("f", 64), 1, "text/plain", "test", otherDeliveries[0].AgentRun.ID, time.Now().UTC()); err == nil {
		t.Fatal("Artifact accepted an AgentRun owned by another Job")
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
	name := investigation.DraftArtifactName(1)
	artifact := spine.Artifact{
		ID: spine.ArtifactID(job.ID, name), JobID: job.ID,
		Name: name, Digest: strings.Repeat("b", 64), ByteSize: 16,
		MediaType: "text/markdown", Producer: "dorf-codebase-investigation",
		AgentRunID: run.ID, CreatedAt: run.FinishedAt,
	}
	stored, created, err := store.RecordCodebaseInvestigationDraft(ctx, artifact)
	if err != nil || !created || stored.JobID != artifact.JobID || stored.ArtifactID != artifact.ID || !stored.CreatedAt.Equal(artifact.CreatedAt) {
		t.Fatalf("Draft=%#v created=%v err=%v", stored, created, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen {
		t.Fatalf("draft prematurely closed Job=%#v err=%v", job, err)
	}
	replayed, created, err := store.RecordCodebaseInvestigationDraft(ctx, artifact)
	if err != nil || created || replayed != stored {
		t.Fatalf("idempotent Draft=%#v created=%v err=%v", replayed, created, err)
	}
	changedArtifact := artifact
	changedArtifact.Digest = strings.Repeat("c", 64)
	if _, _, err := store.RecordCodebaseInvestigationDraft(ctx, changedArtifact); err == nil || !strings.Contains(err.Error(), "immutable retained metadata") {
		t.Fatalf("changed Artifact replay error=%v", err)
	}
	follow, created, err := store.AdmitInvestigationMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "later", Input: "broaden the question"})
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
	secondName := investigation.DraftArtifactName(2)
	secondArtifact := spine.Artifact{ID: spine.ArtifactID(job.ID, secondName), JobID: job.ID, Name: secondName, Digest: strings.Repeat("d", 64), ByteSize: 24, MediaType: "text/markdown", Producer: "dorf-codebase-investigation", AgentRunID: secondRun.ID, CreatedAt: secondRun.FinishedAt}
	if _, created, err := store.RecordCodebaseInvestigationDraft(ctx, secondArtifact); err != nil || !created {
		t.Fatalf("second draft created=%v err=%v", created, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen || job.CleanupState != spine.CleanupPending {
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
	job, created, err := store.AdmitInvestigation(ctx, postgres.NewInvestigationJob{
		NewJob: postgres.NewJob{
			AdmissionKey: "bundle-source-" + fmt.Sprint(time.Now().UnixNano()),
			Workflow:     spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision,
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
	spine.Externals
	mu      sync.Mutex
	job     spine.Job
	turn    spine.HarnessTurn
	effects []spine.ActionKind
}

func (*investigationExternals) Harness() string { return "codex" }
func (e *investigationExternals) effect(kind spine.ActionKind) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.effects = append(e.effects, kind)
	return nil
}
func (e *investigationExternals) SandboxCreate(context.Context, spine.Job, spine.Sandbox) error {
	return e.effect(spine.ActionSandboxCreate)
}
func (e *investigationExternals) RepositoryClone(context.Context, spine.Job, spine.Sandbox, string, string, string) error {
	return e.effect(repository.ActionRepositoryClone)
}
func (e *investigationExternals) RepositoryRestore(_ context.Context, job spine.Job, _ spine.Sandbox, source investigation.Source, contents []byte) error {
	if source.JobID != job.ID || string(contents) != "retained repository input" {
		return fmt.Errorf("unexpected retained repository restore")
	}
	return e.effect(investigation.ActionRepositoryRestore)
}
func (e *investigationExternals) RouteCreate(context.Context, spine.Job, spine.Sandbox, spine.Route) error {
	return e.effect(spine.ActionRouteCreate)
}
func (e *investigationExternals) RouteRevoke(context.Context, spine.Job, spine.Sandbox, spine.Route) error {
	return e.effect(spine.ActionRouteRevoke)
}
func (e *investigationExternals) SandboxDelete(context.Context, spine.Job, spine.Sandbox) error {
	return e.effect(spine.ActionSandboxDelete)
}
func (e *investigationExternals) AgentInitialTurn(_ context.Context, job spine.Job, delivery spine.Delivery, _ string) (spine.HarnessBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.job = job
	e.turn = spine.HarnessTurn{ID: "turn-" + delivery.AgentRun.ID, Status: "completed", Output: "# Finding\n\nThe explicit coordinator is in `internal/workflow/investigation.go`.\n"}
	return spine.HarnessBinding{Harness: "codex", ThreadID: "thread-" + job.ID, Turn: e.turn}, nil
}
func (e *investigationExternals) AgentInitialTurns(context.Context, spine.Job) (spine.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return spine.HarnessHistory{Harness: "codex", ThreadID: "thread-" + e.job.ID, Turns: []spine.HarnessTurn{e.turn}}, nil
}
func (e *investigationExternals) AgentSubmit(_ context.Context, job spine.Job, delivery spine.Delivery, _ string) (spine.HarnessBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if delivery.AgentRun.ThreadID != "thread-"+job.ID {
		return spine.HarnessBinding{}, fmt.Errorf("follow-up did not reuse the investigator Thread")
	}
	e.turn = spine.HarnessTurn{ID: "turn-" + delivery.AgentRun.ID, Status: "completed", Output: "# Revised finding\n\nThe follow-up is grounded in `internal/workflow/investigation.go`.\n"}
	return spine.HarnessBinding{Harness: "codex", ThreadID: delivery.AgentRun.ThreadID, Turn: e.turn}, nil
}
func (e *investigationExternals) AgentTurns(_ context.Context, job spine.Job, threadID string) (spine.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return spine.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: []spine.HarnessTurn{e.turn}}, nil
}
func (*investigationExternals) RepositoryRevision(_ context.Context, _ spine.Job, _ string, revision string) (spine.RevisionObservation, error) {
	now := time.Now().UTC()
	return spine.RevisionObservation{ComparisonBase: revision, Revision: revision, Tree: strings.Repeat("c", 40), StartedAt: now, FinishedAt: now}, nil
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
	execution := spine.NewExecutionService(store, externals, records, nil, absurdruntime.RequireClaim)
	repositoryService := repository.NewService(execution, store, externals, absurdruntime.RequireClaim)
	service := investigation.NewService(repositoryService, store, externals, records, absurdruntime.RequireClaim)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	job, created, err := store.AdmitInvestigation(ctx, postgres.NewInvestigationJob{
		NewJob: postgres.NewJob{
			AdmissionKey: "investigation-terminal-" + suffix,
			Workflow:     spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision,
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
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, params controlplane.JobTaskParams) (controlplane.TaskResultV1, error) {
		work, err := workflow.RunCodebaseInvestigation(taskCtx, service, store, params.JobID)
		if err != nil {
			return controlplane.TaskResultV1{}, err
		}
		if work.Kind != workflow.InvestigationWorkWaitInput {
			return controlplane.TaskResultV1{}, fmt.Errorf("investigation stopped at %s: %s", work.Kind, work.Detail)
		}
		return controlplane.TaskResultV1{JobID: params.JobID, Outcome: "draft-ready"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, controlplane.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, job.CurrentTaskID, spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, spawned.TaskID) })
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "investigation-terminal", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	externals.mu.Lock()
	effects := append([]spine.ActionKind(nil), externals.effects...)
	externals.mu.Unlock()
	wantEffects := []spine.ActionKind{spine.ActionSandboxCreate, investigation.ActionRepositoryRestore, spine.ActionRouteCreate}
	if !slices.Equal(effects, wantEffects) {
		t.Fatalf("effects=%v want=%v", effects, wantEffects)
	}
	drafts, err := store.CodebaseInvestigationDrafts(ctx, job.ID)
	if err != nil || len(drafts) != 1 {
		t.Fatalf("Drafts=%#v err=%v", drafts, err)
	}
	artifacts, err := store.Artifacts(ctx, job.ID)
	if err != nil || len(artifacts) != 1 || artifacts[0].ID != drafts[0].ArtifactID || artifacts[0].Name != investigation.DraftArtifactName(1) {
		t.Fatalf("Artifacts=%#v err=%v", artifacts, err)
	}
	contents, err := records.ReadVerified(artifacts[0].Digest, artifacts[0].ByteSize)
	if err != nil || !strings.Contains(string(contents), "# Finding") {
		t.Fatalf("report=%q err=%v", contents, err)
	}
	allEvidence, err := store.Evidence(ctx, job.ID)
	if err != nil || len(allEvidence) != 0 {
		t.Fatalf("agent prose was recorded as Evidence: %#v err=%v", allEvidence, err)
	}
	artifact, err := store.Artifact(ctx, drafts[0].ArtifactID)
	if err != nil || artifact != artifacts[0] {
		t.Fatalf("Artifact=%#v err=%v want=%#v", artifact, err, artifacts[0])
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || !job.AdmissionOpen || job.CleanupState != spine.CleanupPending {
		t.Fatalf("Job did not remain open for follow-up or cleanup: %#v err=%v", job, err)
	}
	if _, created, err := store.AdmitInvestigationMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "dogfood-follow-up", Input: "Check whether the recommendation still holds after the recent workflow changes."}); err != nil || !created {
		t.Fatalf("follow-up created=%v err=%v", created, err)
	}
	revisionTaskName := taskName + "-follow-up"
	client.MustRegister(absurd.Task(revisionTaskName, func(taskCtx context.Context, params controlplane.JobTaskParams) (controlplane.TaskResultV1, error) {
		work, err := workflow.RunCodebaseInvestigation(taskCtx, service, store, params.JobID)
		if err != nil {
			return controlplane.TaskResultV1{}, err
		}
		if work.Kind != workflow.InvestigationWorkWaitInput {
			return controlplane.TaskResultV1{}, fmt.Errorf("follow-up stopped at %s: %s", work.Kind, work.Detail)
		}
		return controlplane.TaskResultV1{JobID: params.JobID, Outcome: "revised-draft-ready"}, nil
	}))
	revisionTask, err := client.Spawn(ctx, revisionTaskName, controlplane.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: revisionTaskName})
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
	cleaning, err := (controlplane.Application{Store: store, Tasks: client}).RequestCleanup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleaning.AdmissionOpen || cleaning.CleanupState != spine.CleanupScheduled {
		t.Fatalf("explicit cleanup did not close admission and schedule release: %#v", cleaning)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, cleaning.CurrentTaskID) })
	cleanupService := spine.NewExecutionService(store, externals, records, nil, func(context.Context) error { return nil })
	cleaning, sandboxes, err := cleanupService.PrepareCleanup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, owned := range sandboxes {
		for _, kind := range []spine.ActionKind{spine.ActionRouteRevoke, spine.ActionSandboxDelete} {
			action, err := store.GetOrCreateSandboxAction(ctx, owned.ID, kind)
			if err != nil {
				t.Fatal(err)
			}
			if err := cleanupService.ExecuteSandboxAction(ctx, cleaning, owned, action); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.CompleteCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != spine.CleanupComplete {
		t.Fatalf("cleaned Job=%#v err=%v", cleaned, err)
	}
	afterCleanup, err := store.Artifact(ctx, drafts[0].ArtifactID)
	if err != nil || afterCleanup != artifact {
		t.Fatalf("Artifact did not survive cleanup: %#v err=%v", afterCleanup, err)
	}
	afterCleanupContents, err := records.ReadVerified(afterCleanup.Digest, afterCleanup.ByteSize)
	if err != nil || !bytes.Equal(afterCleanupContents, contents) {
		t.Fatalf("Artifact bytes did not survive cleanup: %q err=%v", afterCleanupContents, err)
	}
}
