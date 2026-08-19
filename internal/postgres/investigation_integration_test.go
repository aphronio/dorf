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
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestPostgresCodebaseInvestigationIdentityAndTypedReport(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("codebase-investigation-%d", time.Now().UnixNano())
	input := postgres.NewJob{
		AdmissionKey: key, Workflow: spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision,
		Goal: "Find one unnecessary coding-workflow dependency.", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("a", 40), Branch: "dorf/investigation-test",
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		InvestigationSource: spine.CodebaseInvestigationSource{Kind: spine.InvestigationSourceRemote, Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40)},
	}
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created || job.Workflow != input.Workflow || job.WorkflowRevision != input.WorkflowRevision {
		t.Fatalf("Job=%#v created=%v err=%v", job, created, err)
	}
	repeated, created, err := store.Admit(ctx, input)
	if err != nil || created || repeated.ID != job.ID {
		t.Fatalf("idempotent Job=%#v created=%v err=%v", repeated, created, err)
	}
	source, err := store.CodebaseInvestigationSource(ctx, job.ID)
	if err != nil || source.JobID != job.ID || source.Kind != spine.InvestigationSourceRemote || source.Repository != input.Repository || source.Revision != input.Revision {
		t.Fatalf("source=%#v err=%v", source, err)
	}
	changedSource := input
	changedSource.Repository = ""
	changedSource.InvestigationSource = spine.CodebaseInvestigationSource{
		Kind: spine.InvestigationSourceGitBundle, Revision: input.Revision,
		BundleDigest: strings.Repeat("e", 64), BundleByteSize: 123,
	}
	if _, _, err := store.Admit(ctx, changedSource); err == nil || !strings.Contains(err.Error(), "different complete Job input") {
		t.Fatalf("same admission key changed source identity: %v", err)
	}
	changed := input
	changed.Workflow = spine.WorkflowCodingToProposal
	changed.WorkflowRevision = spine.CodingToProposalRevision
	changed.GitHubRepository, changed.GitHubInstallation, changed.BaseBranch = "aphronio/dorf", "42", "main"
	if _, _, err := store.Admit(ctx, changed); err == nil {
		t.Fatal("same admission key changed workflow identity")
	}
	if _, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: spine.MessageFromHuman, FromID: "later", Input: "broaden the question"}); err == nil || created {
		t.Fatalf("investigation accepted unsupported follow-up: created=%v err=%v", created, err)
	}

	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].AgentRun.Role != "investigate" || deliveries[0].AgentRun.Capability != "repository-read-report" {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	run := deliveries[0].AgentRun
	otherInput := input
	otherInput.AdmissionKey += "-other"
	otherInput.Branch += "-other"
	otherJob, created, err := store.Admit(ctx, otherInput)
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
	artifact := spine.Artifact{
		ID: spine.ArtifactID(job.ID, spine.CodebaseInvestigationReportArtifactName), JobID: job.ID,
		Name: spine.CodebaseInvestigationReportArtifactName, Digest: strings.Repeat("b", 64), ByteSize: 16,
		MediaType: "text/markdown", Producer: "dorf-codebase-investigation",
		AgentRunID: run.ID, CreatedAt: run.FinishedAt,
	}
	stored, created, err := store.RecordCodebaseInvestigationReport(ctx, artifact)
	if err != nil || !created || stored.JobID != artifact.JobID || stored.ReportArtifactID != artifact.ID || !stored.ObservedAt.Equal(artifact.CreatedAt) {
		t.Fatalf("Report=%#v created=%v err=%v", stored, created, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.AdmissionOpen {
		t.Fatalf("terminal Job=%#v err=%v", job, err)
	}
	replayed, created, err := store.RecordCodebaseInvestigationReport(ctx, artifact)
	if err != nil || created || replayed != stored {
		t.Fatalf("idempotent Report=%#v created=%v err=%v", replayed, created, err)
	}
	changedArtifact := artifact
	changedArtifact.Digest = strings.Repeat("c", 64)
	if _, _, err := store.RecordCodebaseInvestigationReport(ctx, changedArtifact); err == nil || !strings.Contains(err.Error(), "immutable retained metadata") {
		t.Fatalf("changed Artifact replay error=%v", err)
	}
}

func TestPostgresCodebaseInvestigationRetainsBundleSourceIdentity(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	revision := strings.Repeat("9", 40)
	supplied := spine.CodebaseInvestigationSource{
		Kind: spine.InvestigationSourceGitBundle, Revision: revision,
		BundleDigest: strings.Repeat("8", 64), BundleByteSize: 4096,
	}
	job, created, err := store.Admit(ctx, postgres.NewJob{
		AdmissionKey: "bundle-source-" + fmt.Sprint(time.Now().UnixNano()),
		Workflow:     spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision,
		Goal: "Inspect an unpublished commit.", Revision: revision, Branch: "dorf/local-investigation",
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		InvestigationSource: supplied,
	})
	if err != nil || !created || job.Repository != "" || job.Revision != revision {
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
	spine.ServiceExternals
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
func (e *investigationExternals) RepositoryClone(context.Context, spine.Job, spine.Sandbox) error {
	return e.effect(spine.ActionRepositoryClone)
}
func (e *investigationExternals) RepositoryRestore(_ context.Context, job spine.Job, _ spine.Sandbox, source spine.CodebaseInvestigationSource, contents []byte) error {
	if source.JobID != job.ID || source.Revision != job.Revision || string(contents) != "retained repository input" {
		return fmt.Errorf("unexpected retained repository restore")
	}
	return e.effect(spine.ActionRepositoryRestore)
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
func (e *investigationExternals) AgentInitialTurn(_ context.Context, job spine.Job, delivery spine.Delivery) (spine.HarnessBinding, error) {
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
func (e *investigationExternals) AgentTurns(_ context.Context, job spine.Job, threadID string) (spine.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return spine.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: []spine.HarnessTurn{e.turn}}, nil
}
func (*investigationExternals) RepositoryRevision(_ context.Context, job spine.Job) (spine.RevisionObservation, error) {
	now := time.Now().UTC()
	return spine.RevisionObservation{ComparisonBase: job.Revision, Revision: job.Revision, Tree: strings.Repeat("c", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}, nil
}

func TestPostgresCodebaseInvestigationCoordinatorReachesReportAndCleanup(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	records := blob.Store{Root: t.TempDir()}
	retained, err := records.Put([]byte("retained repository input"))
	if err != nil {
		t.Fatal(err)
	}
	externals := &investigationExternals{}
	service := spine.NewService(store, externals, records, nil, absurdruntime.RequireClaim)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	job, created, err := store.Admit(ctx, postgres.NewJob{
		AdmissionKey: "investigation-terminal-" + suffix,
		Workflow:     spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision,
		Goal:     "Find one concrete simplification.",
		Revision: strings.Repeat("d", 40), Branch: "dorf/investigation-terminal-" + suffix,
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		InvestigationSource: spine.CodebaseInvestigationSource{
			Kind: spine.InvestigationSourceGitBundle, Revision: strings.Repeat("d", 40),
			BundleDigest: retained.Digest, BundleByteSize: retained.ByteSize,
		},
	})
	if err != nil || !created {
		t.Fatalf("Job=%#v created=%v err=%v", job, created, err)
	}
	taskName := "test-codebase-investigation-" + suffix
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, params workflow.Params) (workflow.TaskResultV1, error) {
		work, err := workflow.RunCodebaseInvestigation(taskCtx, service, store, params.JobID)
		if err != nil {
			return workflow.TaskResultV1{}, err
		}
		if work.Kind != workflow.InvestigationWorkComplete {
			return workflow.TaskResultV1{}, fmt.Errorf("investigation stopped at %s: %s", work.Kind, work.Detail)
		}
		return workflow.TaskResultV1{JobID: params.JobID, Outcome: "report-recorded"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, workflow.Params{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName})
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
	wantEffects := []spine.ActionKind{spine.ActionSandboxCreate, spine.ActionRepositoryRestore, spine.ActionRouteCreate}
	if !slices.Equal(effects, wantEffects) {
		t.Fatalf("effects=%v want=%v", effects, wantEffects)
	}
	report, err := store.CodebaseInvestigationReport(ctx, job.ID)
	if err != nil || report == nil {
		t.Fatalf("Report=%#v err=%v", report, err)
	}
	artifacts, err := store.Artifacts(ctx, job.ID)
	if err != nil || len(artifacts) != 1 || artifacts[0].ID != report.ReportArtifactID || artifacts[0].Name != spine.CodebaseInvestigationReportArtifactName {
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
	artifact, err := store.Artifact(ctx, report.ReportArtifactID)
	if err != nil || artifact != artifacts[0] {
		t.Fatalf("Artifact=%#v err=%v want=%#v", artifact, err, artifacts[0])
	}

	cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, cleaning.CurrentTaskID) })
	cleanupService := spine.NewService(store, externals, records, nil, func(context.Context) error { return nil })
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
	afterCleanup, err := store.Artifact(ctx, report.ReportArtifactID)
	if err != nil || afterCleanup != artifact {
		t.Fatalf("Artifact did not survive cleanup: %#v err=%v", afterCleanup, err)
	}
	afterCleanupContents, err := records.ReadVerified(afterCleanup.Digest, afterCleanup.ByteSize)
	if err != nil || !bytes.Equal(afterCleanupContents, contents) {
		t.Fatalf("Artifact bytes did not survive cleanup: %q err=%v", afterCleanupContents, err)
	}
}
