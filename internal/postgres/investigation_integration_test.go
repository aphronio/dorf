package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/evidence"
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
	}
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created || job.Workflow != input.Workflow || job.WorkflowRevision != input.WorkflowRevision {
		t.Fatalf("Job=%#v created=%v err=%v", job, created, err)
	}
	repeated, created, err := store.Admit(ctx, input)
	if err != nil || created || repeated.ID != job.ID {
		t.Fatalf("idempotent Job=%#v created=%v err=%v", repeated, created, err)
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
	evidence := spine.Evidence{
		ID: spine.EvidenceID(run.ID, "investigation-report"), Digest: strings.Repeat("b", 64), ByteSize: 16,
		MediaType: "text/markdown", Producer: "dorf-codebase-investigation", Kind: "investigation-report",
		AgentRunID: run.ID, Revision: job.Revision, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
	}
	receipt := spine.CodebaseInvestigationReport{
		JobID: job.ID, AgentRunID: run.ID,
		ReportEvidenceID: evidence.ID, ObservedAt: run.FinishedAt,
	}
	stored, created, err := store.RecordCodebaseInvestigationReport(ctx, receipt, evidence)
	if err != nil || !created || stored.JobID != receipt.JobID || stored.AgentRunID != receipt.AgentRunID || stored.ReportEvidenceID != receipt.ReportEvidenceID || !stored.ObservedAt.Equal(receipt.ObservedAt) {
		t.Fatalf("Report=%#v created=%v err=%v", stored, created, err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.AdmissionOpen {
		t.Fatalf("terminal Job=%#v err=%v", job, err)
	}
	receipt.ObservedAt = receipt.ObservedAt.Add(time.Hour)
	replayed, created, err := store.RecordCodebaseInvestigationReport(ctx, receipt, evidence)
	if err != nil || created || replayed.JobID != stored.JobID || replayed.AgentRunID != stored.AgentRunID || replayed.ReportEvidenceID != stored.ReportEvidenceID || !replayed.ObservedAt.Equal(stored.ObservedAt) {
		t.Fatalf("idempotent Report=%#v created=%v err=%v", replayed, created, err)
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
	records := evidence.Store{Root: t.TempDir()}
	externals := &investigationExternals{}
	service := spine.NewService(store, externals, records, nil, absurdruntime.RequireClaim)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	job, created, err := store.Admit(ctx, postgres.NewJob{
		AdmissionKey: "investigation-terminal-" + suffix,
		Workflow:     spine.WorkflowCodebaseInvestigation, WorkflowRevision: spine.CodebaseInvestigationRevision,
		Goal: "Find one concrete simplification.", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("d", 40), Branch: "dorf/investigation-terminal-" + suffix,
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
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
	if err := store.AttachMessageTask(ctx, job.ID, spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, spawned.TaskID) })
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "investigation-terminal", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	report, err := store.CodebaseInvestigationReport(ctx, job.ID)
	if err != nil || report == nil {
		t.Fatalf("Report=%#v err=%v", report, err)
	}
	allEvidence, err := store.Evidence(ctx, job.ID)
	if err != nil || len(allEvidence) != 1 || allEvidence[0].ID != report.ReportEvidenceID {
		t.Fatalf("Evidence=%#v err=%v", allEvidence, err)
	}
	contents, err := records.ReadVerified(allEvidence[0].Digest, allEvidence[0].ByteSize)
	if err != nil || !strings.Contains(string(contents), "# Finding") {
		t.Fatalf("report=%q err=%v", contents, err)
	}

	cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, cleaning.CleanupTaskID) })
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
}
