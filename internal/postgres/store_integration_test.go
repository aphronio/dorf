package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	profileapp "github.com/aphronio/dorf/internal/profile"
	policy "github.com/aphronio/dorf/internal/review"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type providerCheck struct {
	err error
}

func codingDelivery(ctx context.Context, store postgres.Store, jobID string) (*core.Delivery, error) {
	work, err := store.CodingAgentMessage(ctx, jobID)
	if err != nil || work == nil {
		return nil, err
	}
	execution, err := store.AgentMessageExecution(ctx, work.MessageID)
	if err != nil {
		return nil, err
	}
	return &core.Delivery{Message: execution.Message, AgentRun: execution.AgentRun}, nil
}

func (p providerCheck) Check(context.Context, string) error { return p.err }

type failOnceWorkflowBarrier struct {
	mu     sync.Mutex
	point  string
	failed bool
}

func (*failOnceWorkflowBarrier) Reach(context.Context, string, core.Delivery) error { return nil }

func (b *failOnceWorkflowBarrier) ReachWorkflow(_ context.Context, point, _, _ string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if point == b.point && !b.failed {
		b.failed = true
		return errors.New("lost provider success before durable Action receipt")
	}
	return nil
}

type blockingCreateExternals struct {
	*integrationExternals
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingCreateExternals) SandboxCreate(ctx context.Context, job core.Job, sandbox core.Sandbox) error {
	if err := e.integrationExternals.SandboxCreate(ctx, job, sandbox); err != nil {
		return err
	}
	e.once.Do(func() { close(e.entered) })
	select {
	case <-e.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type integrationExecution interface {
	core.Execution
	core.AgentReconciliation
	core.CleanupExecution
}

type integrationRuntimeResolver struct {
	execution            integrationExecution
	files                core.SandboxFileReader
	profile              profileapp.Runtime
	codingRuntime        coding.Runtime
	investigationRuntime investigation.Runtime
}

func (r integrationRuntimeResolver) ResolveSandbox(_ context.Context, name string) (core.SandboxRuntime, error) {
	if name != r.profile.SandboxProfile {
		return core.SandboxRuntime{}, fmt.Errorf("unexpected Sandbox profile %q", name)
	}
	return core.SandboxRuntime{Execution: r.execution, Files: r.files, SandboxProfile: r.profile.SandboxProfile}, nil
}

func (r integrationRuntimeResolver) ResolveCleanup(_ context.Context, name string) (core.CleanupRuntime, error) {
	if name != r.profile.SandboxProfile {
		return core.CleanupRuntime{}, fmt.Errorf("unexpected Sandbox profile %q", name)
	}
	return core.CleanupRuntime{Execution: r.execution, SandboxProfile: r.profile.SandboxProfile}, nil
}

func (r integrationRuntimeResolver) ResolveCoding(_ context.Context, name string) (coding.Runtime, error) {
	if name != r.codingRuntime.Profile.SandboxProfile {
		return coding.Runtime{}, fmt.Errorf("unexpected Sandbox profile %q", name)
	}
	return r.codingRuntime, nil
}

func (r integrationRuntimeResolver) ResolveInvestigation(_ context.Context, name string) (investigation.Runtime, error) {
	if name != r.investigationRuntime.Profile.SandboxProfile {
		return investigation.Runtime{}, fmt.Errorf("unexpected Sandbox profile %q", name)
	}
	return r.investigationRuntime, nil
}

func testDatabase(t *testing.T) (*sql.DB, postgres.Store, *absurd.Client) {
	t.Helper()
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
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
	profile, _, err := store.CreateSandboxProfile(context.Background(), core.SandboxProfile{
		Name: "incus", Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("a", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if !profile.BaseVerified() {
		_, verification, err := store.BeginSandboxProfileVerification(context.Background(), profile.Name)
		if err == nil {
			err = store.RecordSandboxProfileProbe(context.Background(), verification, "codex-test")
		}
		if err == nil {
			err = store.RecordSandboxProfileVerificationCleanup(context.Background(), verification)
		}
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	queueName := fmt.Sprintf("%s_test_%d", config.QueueName, time.Now().UnixNano())
	client, err := absurd.New(absurd.Options{DB: db, QueueName: queueName})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := client.CreateQueue(context.Background(), queueName); err != nil {
		client.Close()
		db.Close()
		t.Fatal(err)
	}
	externals := &integrationExternals{}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
	workspaceExecutor := gitworkspace.NewExecutor(execution, externals, nil)
	codingService := coding.NewService(workspaceExecutor, store, externals, blob.Store{}, func(context.Context) error { return nil })
	runtimeProfile := profileapp.Runtime{SandboxProfile: "incus"}
	investigationService := investigation.NewService(workspaceExecutor, externals, blob.Store{})
	resolver := integrationRuntimeResolver{
		execution:            execution,
		profile:              runtimeProfile,
		codingRuntime:        coding.Runtime{Profile: runtimeProfile, Agent: execution, Coding: codingService},
		investigationRuntime: investigation.Runtime{Profile: runtimeProfile, Agent: execution, Investigation: investigationService},
	}
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver, CleanupRuntimes: resolver}
	application.RegisterCleanup()
	coding.Register(application, store, resolver)
	investigation.Register(application, store, resolver)
	t.Cleanup(func() {
		if err := client.DropQueue(context.Background(), queueName); err != nil {
			t.Errorf("drop test queue %q: %v", queueName, err)
		}
		client.Close()
		db.Close()
	})
	return db, store, client
}

func TestActiveWorkerRecoversOrphanedCleanupRequestAndScheduledReplayIsInert(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("cleanup-request-recovery-%d", time.Now().UnixNano())
	job, created, err := store.AdmitCoding(ctx, codingJobInput(key, "recover explicit cleanup scheduling", "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", "dorf/cleanup-recovery"))
	if err != nil || !created {
		t.Fatalf("admit Job=%#v created=%t err=%v", job, created, err)
	}
	application := core.Application{Store: store, Tasks: client}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	recoveryDone := make(chan error, 1)
	go func() {
		workerDone <- client.RunWorker(workerCtx, absurd.WorkerOptions{WorkerID: "cleanup-request-recovery", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 1})
	}()
	go func() { recoveryDone <- application.ReconcileCleanupRequests(workerCtx, 100*time.Millisecond) }()
	t.Cleanup(func() {
		stopWorker()
		<-workerDone
		<-recoveryDone
	})

	// This is the crash boundary: the request is durable, but no caller Spawns
	// or attaches cleanup. The already-running recovery loop must do so.
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	requested, err := store.Job(ctx, job.ID)
	if err != nil || requested.CleanupState != core.CleanupRequested || requested.CurrentTaskID != "" || requested.AdmissionOpen {
		t.Fatalf("durable cleanup request=%#v err=%v", requested, err)
	}
	var scheduled core.Job
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		scheduled, err = store.Job(ctx, job.ID)
		if err == nil && scheduled.CurrentTaskID != "" && scheduled.CleanupState != core.CleanupRequested {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || scheduled.CurrentTaskID == "" || (scheduled.CleanupState != core.CleanupScheduled && scheduled.CleanupState != core.CleanupComplete) {
		t.Fatalf("continuously recovered cleanup=%#v err=%v", scheduled, err)
	}
	handle, err := application.OpenJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.RequestCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.Job(ctx, job.ID)
	if err != nil || replayed.CurrentTaskID != scheduled.CurrentTaskID || (replayed.CleanupState != core.CleanupScheduled && replayed.CleanupState != core.CleanupComplete) {
		t.Fatalf("scheduled cleanup replay=%#v err=%v", replayed, err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), scheduled.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		execution, _ := client.FetchTaskResult(ctx, client.QueueName(), scheduled.CurrentTaskID)
		t.Fatalf("recovered cleanup completion=%#v err=%v task=%#v", cleaned, err, execution)
	}
}

func TestWorkflowEnsureAndCleanupSerializeBothWinnerOrders(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	externals := &blockingCreateExternals{
		integrationExternals: &integrationExternals{}, entered: make(chan struct{}), release: make(chan struct{}),
	}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(codingAgentExecution{store: store, externals: externals.integrationExternals})
	workspace := gitworkspace.NewExecutor(execution, externals, nil)
	service := coding.NewService(workspace, store, externals, blob.Store{}, absurdruntime.RequireClaim)
	profile := profileapp.Runtime{SandboxProfile: "incus"}
	resolver := integrationRuntimeResolver{
		execution: execution, profile: profile,
		codingRuntime: coding.Runtime{Profile: profile, Agent: execution, Coding: service},
	}
	client := newFaultClient(t, store, fmt.Sprintf("dorf_workflow_cleanup_race_%d", time.Now().UnixNano()))
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver, CleanupRuntimes: resolver}
	application.RegisterCleanup()
	coding.Register(application, store, resolver)
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- client.RunWorker(workerCtx, absurd.WorkerOptions{WorkerID: "workflow-cleanup-race", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 1})
	}()
	t.Cleanup(func() { stopWorker(); <-workerDone })

	job, created, err := store.AdmitCoding(ctx, codingJobInput(
		fmt.Sprintf("ensure-wins-%d", time.Now().UnixNano()), "ensure wins the effect fence",
		"2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", "dorf/ensure-wins",
	))
	if err != nil || !created {
		t.Fatalf("admit ensure winner created=%t err=%v", created, err)
	}
	if err := application.ScheduleJobTask(ctx, job, coding.TaskName, coding.TaskKey(job.ID)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-externals.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("workflow Sandbox ensure did not enter provider")
	}
	handle, err := application.OpenJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- handle.RequestCleanup(ctx) }()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup crossed active Sandbox provider fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(externals.release)
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	cleaning, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), cleaning.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("cleanup did not inventory winning ensure: %#v err=%v", cleaned, err)
	}
	got := externals.effectKinds()
	if len(got) < 3 || got[0] != core.ActionSandboxCreate || got[len(got)-2] != core.ActionRouteRevoke || got[len(got)-1] != core.ActionSandboxDelete {
		t.Fatalf("ensure-winner provider effects=%v", got)
	}

	loserExternals := &integrationExternals{}
	loserExecution := core.NewExecutionService(store, loserExternals, nil, absurdruntime.RequireClaim)
	loserWorkspace := gitworkspace.NewExecutor(loserExecution, loserExternals, nil)
	loserService := coding.NewService(loserWorkspace, store, loserExternals, blob.Store{}, absurdruntime.RequireClaim)
	loserResolver := integrationRuntimeResolver{
		execution: loserExecution, profile: profile,
		codingRuntime: coding.Runtime{Profile: profile, Agent: loserExecution, Coding: loserService},
	}
	loserClient := newFaultClient(t, store, fmt.Sprintf("dorf_cleanup_wins_%d", time.Now().UnixNano()))
	loserApplication := core.Application{Store: store, Tasks: loserClient, SandboxRuntimes: loserResolver, CleanupRuntimes: loserResolver}
	loserApplication.RegisterCleanup()
	coding.Register(loserApplication, store, loserResolver)
	loser, created, err := store.AdmitCoding(ctx, codingJobInput(
		fmt.Sprintf("cleanup-wins-%d", time.Now().UnixNano()), "cleanup wins before ensure",
		"2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", "dorf/cleanup-wins",
	))
	if err != nil || !created {
		t.Fatalf("admit cleanup winner created=%t err=%v", created, err)
	}
	if err := loserApplication.ScheduleJobTask(ctx, loser, coding.TaskName, coding.TaskKey(loser.ID)); err != nil {
		t.Fatal(err)
	}
	loserHandle, err := loserApplication.OpenJob(ctx, loser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := loserHandle.RequestCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if got := loserExternals.effectKinds(); len(got) != 0 {
		t.Fatalf("cleanup winner allowed provider effect before worker start: %v", got)
	}
}

func codingJobInput(key, goal, revision, branch string) coding.Admission {
	return coding.Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: key, Goal: goal, SandboxProfile: "incus", ProviderConnection: "primary",
			Model: "gpt-5.6-sol", ReasoningEffort: "high",
		},
		Repository: "https://github.com/aphronio/dorf.git", Revision: revision, Branch: branch,
		GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield",
	}
}

func requestCleanupIntegration(t *testing.T, application core.Application, jobID string) core.Job {
	t.Helper()
	handle, err := application.OpenJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.RequestCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := application.Store.Job(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestPostgresMessageIdempotencyConcurrentFIFOAndLowestUnsettled(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	var migrationCount int
	var migrationName string
	if err := db.QueryRowContext(ctx, `select count(*),min(name) from dorf.schema_migrations`).Scan(&migrationCount, &migrationName); err != nil || migrationCount != 1 || migrationName != "001_baseline.sql" {
		t.Fatalf("baseline migrations count=%d name=%q err=%v", migrationCount, migrationName, err)
	}
	key := fmt.Sprintf("message-integration-%d", time.Now().UnixNano())
	input := codingJobInput(key, "initial input", "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", "dorf/integration")
	blocked := input
	blocked.AdmissionKey += "-provider-blocked"
	if _, _, err := coding.Admit(ctx, store, core.Application{Store: store, Tasks: client}, providerCheck{err: errors.New("provider is not ready")}, profileapp.Runtime{SandboxProfile: blocked.SandboxProfile}, blocked); err == nil {
		t.Fatal("new Job bypassed provider readiness")
	}
	if _, err := store.Job(ctx, core.JobID(blocked.AdmissionKey)); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("failed provider preflight persisted Job: %v", err)
	}
	job, created, err := coding.Admit(ctx, store, core.Application{Store: store, Tasks: client}, providerCheck{}, profileapp.Runtime{SandboxProfile: input.SandboxProfile}, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	if job.SandboxProfile != "incus" || job.Workflow != coding.Workflow || job.WorkflowRevision != coding.WorkflowRevision {
		t.Fatalf("admitted Job profile/Workflow=%#v", job)
	}
	repeatedJob, created, err := coding.Admit(ctx, store, core.Application{Store: store, Tasks: client}, providerCheck{err: errors.New("Gateway unavailable during retry")}, profileapp.Runtime{SandboxProfile: input.SandboxProfile}, input)
	if err != nil || created || repeatedJob.ID != job.ID || repeatedJob.CurrentTaskID != job.CurrentTaskID {
		t.Fatalf("idempotent Job admission=%#v created=%v err=%v", repeatedJob, created, err)
	}
	changedJob := input
	changedJob.Goal = "changed complete input"
	if _, _, err := coding.Admit(ctx, store, core.Application{Store: store, Tasks: client}, providerCheck{err: errors.New("Gateway unavailable during retry")}, profileapp.Runtime{SandboxProfile: changedJob.SandboxProfile}, changedJob); err == nil {
		t.Fatal("changed complete Job input under the same admission key did not conflict")
	}
	changedProfile := input
	changedProfile.SandboxProfile = "e2b"
	if _, _, err := coding.Admit(ctx, store, core.Application{Store: store, Tasks: client}, providerCheck{err: errors.New("Gateway unavailable during retry")}, profileapp.Runtime{SandboxProfile: changedProfile.SandboxProfile}, changedProfile); err == nil {
		t.Fatal("changed Sandbox profile under the same admission key did not conflict")
	}
	taskIDs := []string{job.CurrentTaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), client.QueueName(), id)
		}
	})

	first, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "client-retry", Input: "same text"})
	if err != nil || !created || first.Sequence != 2 || first.FromKind != "human" || first.FromID != "client-retry" || first.ID != core.MessageID(job.ID, "human", "client-retry") {
		t.Fatalf("first message=%#v created=%v err=%v", first, created, err)
	}
	if _, created, err := store.AdmitInvestigationMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "wrong-workflow", Input: "must not cross workflow authority"}); err == nil || created || !strings.Contains(err.Error(), "is not codebase-investigation") {
		t.Fatalf("investigation admission crossed into coding: created=%v err=%v", created, err)
	}
	repeated, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "client-retry", Input: "same text"})
	if err != nil || created || repeated != first {
		t.Fatalf("idempotent message=%#v created=%v err=%v", repeated, created, err)
	}
	if _, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.NamedSandboxID(job.ID, "other"), FromKind: "human", FromID: "client-retry", Input: "same text"}); err == nil || created || !strings.Contains(err.Error(), "different complete Sandbox delivery request") {
		t.Fatalf("same send key replayed through another Sandbox: created=%v err=%v", created, err)
	}
	if _, _, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "client-retry", Input: "changed"}); err == nil {
		t.Fatal("changed input under the same source identity did not conflict")
	}
	if _, _, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "client-retry", Input: "same text "}); err == nil {
		t.Fatal("byte-distinct complete input under the same source identity did not conflict")
	}
	distinct, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "client-distinct", Input: "same text"})
	if err != nil || !created || distinct.ID == first.ID || distinct.Sequence != 3 {
		t.Fatalf("distinct identical message=%#v created=%v err=%v", distinct, created, err)
	}
	crossKind, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "workflow", FromID: distinct.FromID, Input: "same source identity from the workflow"})
	if err != nil || !created || crossKind.Sequence != 4 || crossKind.ID == distinct.ID || crossKind.ID != core.MessageID(job.ID, "workflow", distinct.FromID) || crossKind.FromKind != "workflow" || crossKind.FromID != distinct.FromID {
		t.Fatalf("cross-kind source identity=%#v created=%v err=%v", crossKind, created, err)
	}

	const concurrent = 12
	sequences := make(chan int64, concurrent)
	errors := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := range concurrent {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			message, _, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: fmt.Sprintf("concurrent-%02d", i), Input: "same concurrent text"})
			if err == nil {
				sequences <- message.Sequence
			}
			errors <- err
		}(i)
	}
	wg.Wait()
	close(sequences)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var got []int
	for sequence := range sequences {
		got = append(got, int(sequence))
	}
	sort.Ints(got)
	for i, sequence := range got {
		if sequence != i+5 {
			t.Fatalf("concurrent FIFO positions=%v", got)
		}
	}

	threadID := "thread-" + job.ID
	delivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || delivery.Message.Sequence != 1 {
		t.Fatalf("lowest delivery=%#v err=%v", delivery, err)
	}
	if delivery.AgentRun.SandboxID != core.MainSandboxName(job.ID) {
		t.Fatalf("delivery Sandbox=%q want=%q", delivery.AgentRun.SandboxID, core.MainSandboxName(job.ID))
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", threadID, "turn-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	next, err := codingDelivery(ctx, store, job.ID)
	if err != nil || next.Message.Sequence != 2 || next.AgentRun.ID == delivery.AgentRun.ID {
		t.Fatalf("next delivery=%#v err=%v", next, err)
	}
	if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", "turn-"+job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-2-"+job.ID, "running"); err != nil {
		t.Fatal(err)
	}
	blockers, err := store.UnsettledAgentMessages(ctx, job.ID)
	if err != nil || len(blockers) != 1 || blockers[0].MessageID != next.Message.ID || blockers[0].SandboxID != core.MainSandboxName(job.ID) {
		t.Fatalf("active harness mutations=%#v err=%v", blockers, err)
	}
	stillOpen, err := store.Job(ctx, job.ID)
	if err != nil || !stillOpen.AdmissionOpen {
		t.Fatalf("harness mutation inspection changed admission: %#v err=%v", stillOpen, err)
	}
	if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-2-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	fenceEntered := make(chan struct{})
	releaseFence := make(chan struct{})
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- store.WithJobFence(ctx, job.ID, func() error {
			close(fenceEntered)
			<-releaseFence
			return nil
		})
	}()
	<-fenceEntered
	type cleanupResult struct {
		job core.Job
		err error
	}
	cleanupDone := make(chan cleanupResult, 1)
	go func() {
		application := core.Application{Store: store, Tasks: client}
		handle, err := application.OpenJob(ctx, job.ID)
		if err == nil {
			err = handle.RequestCleanup(ctx)
		}
		var cleaning core.Job
		if err == nil {
			cleaning, err = store.Job(ctx, job.ID)
		}
		cleanupDone <- cleanupResult{job: cleaning, err: err}
	}()
	select {
	case result := <-cleanupDone:
		close(releaseFence)
		t.Fatalf("cleanup crossed the active harness-mutation fence: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFence)
	if err := <-fenceDone; err != nil {
		t.Fatal(err)
	}
	cleanup := <-cleanupDone
	if cleanup.err != nil {
		t.Fatal(cleanup.err)
	}
	cleaning := cleanup.job
	taskIDs = append(taskIDs, cleaning.CurrentTaskID)
	if cleaning.AdmissionOpen {
		t.Fatal("cleanup did not durably close admission")
	}
	if retry, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "client-retry", Input: "same text"}); err != nil || created || retry != first {
		t.Fatalf("closed admission did not preserve idempotent retry: %#v %v %v", retry, created, err)
	}
	if _, _, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "after-cleanup", Input: "late"}); err == nil {
		t.Fatal("cleanup allowed a new message")
	}
}

func TestSandboxProfileVerificationHasOneOwnerAndReleasesAfterCrash(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("verify-owner-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("d", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
	}); err != nil {
		t.Fatal(err)
	}

	type verificationResult struct {
		verification core.ProfileVerification
		err          error
	}
	entered := make(chan verificationResult, 1)
	release := make(chan struct{})
	firstDone := make(chan verificationResult, 1)
	go func() {
		var attempt core.ProfileVerification
		err := store.WithSandboxProfileVerification(ctx, name, func(ctx context.Context) error {
			_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
			if err != nil {
				entered <- verificationResult{err: err}
				return err
			}
			attempt = verification
			entered <- verificationResult{verification: verification}
			<-release
			return errors.New("verification worker stopped")
		})
		firstDone <- verificationResult{verification: attempt, err: err}
	}()
	started := <-entered
	if started.err != nil {
		close(release)
		t.Fatal(started.err)
	}
	first := started.verification
	contenderRan := false
	if err := store.WithSandboxProfileVerification(ctx, name, func(context.Context) error {
		contenderRan = true
		return nil
	}); err == nil || !strings.Contains(err.Error(), "already running") || contenderRan {
		close(release)
		t.Fatalf("concurrent verification ran=%v err=%v", contenderRan, err)
	}

	close(release)
	stopped := <-firstDone
	if stopped.err == nil || !strings.Contains(stopped.err.Error(), "worker stopped") || stopped.verification != first {
		t.Fatalf("stopped verification=%#v err=%v", stopped.verification, stopped.err)
	}

	var resumed core.ProfileVerification
	if err := store.WithSandboxProfileVerification(ctx, name, func(ctx context.Context) error {
		_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
		if err != nil {
			return err
		}
		resumed = verification
		if verification.ProfileName != first.ProfileName || verification.ContractVersion != first.ContractVersion || verification.SandboxID != first.SandboxID || verification.OwnershipNonce != first.OwnershipNonce {
			return fmt.Errorf("resumed a different verification attempt: first=%#v resumed=%#v", first, verification)
		}
		if err := store.RecordSandboxProfileProbe(ctx, verification, "codex resumed"); err != nil {
			return err
		}
		return store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}); err != nil {
		t.Fatal(err)
	}
	input := codingJobInput("verification-fence-"+name, "wait for the exact profile proof", strings.Repeat("a", 40), "dorf/verification-fence")
	input.SandboxProfile = name
	job, created, err := store.AdmitCoding(ctx, input)
	if err != nil || !created || job.SandboxProfile != name || resumed.OwnershipNonce != first.OwnershipNonce {
		t.Fatalf("admission after resumed verification Job=%#v created=%v resumed=%#v err=%v", job, created, resumed, err)
	}
}

func TestSandboxProfileVerificationTransitionSerializesNewAdmission(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("verify-serial-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("f", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
	}); err != nil {
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex serial")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}

	transition, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transition.Rollback()
	var locked string
	if err := transition.QueryRowContext(ctx, `select name from dorf.sandbox_profiles where name=$1 for update`, name).Scan(&locked); err != nil || locked != name {
		t.Fatalf("lock profile=%q err=%v", locked, err)
	}
	input := codingJobInput("verification-serialization-"+name, "serialize against verification", strings.Repeat("a", 40), "dorf/verification-serialization")
	input.SandboxProfile = name
	type admissionResult struct {
		created bool
		err     error
	}
	started := make(chan struct{})
	admitted := make(chan admissionResult, 1)
	go func() {
		close(started)
		_, created, err := store.AdmitCoding(ctx, input)
		admitted <- admissionResult{created: created, err: err}
	}()
	<-started
	select {
	case result := <-admitted:
		t.Fatalf("admission crossed the locked verification transition: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := transition.ExecContext(ctx, `delete from dorf.sandbox_profile_verifications where profile_name=$1`, name); err != nil {
		t.Fatal(err)
	}
	nonce := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	if _, err := transition.ExecContext(ctx, `insert into dorf.sandbox_profile_verifications(profile_name,contract_version,sandbox_id,ownership_nonce) values($1,$2,$3,$4)`, name, core.BaseProfileContract, "transition-"+name, nonce); err != nil {
		t.Fatal(err)
	}
	if err := transition.Commit(); err != nil {
		t.Fatal(err)
	}
	result := <-admitted
	if result.created || result.err == nil || !strings.Contains(result.err.Error(), core.BaseProfileContract) {
		t.Fatalf("admission after verification transition=%#v", result)
	}
}

func TestSandboxProfilesAreVerifiedDefaultedAndImmutableWhileInUse(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("managed-%d", time.Now().UnixNano())
	profile := core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderE2B, Harness: "pi", Artifact: "dorf:exact-build",
		E2BGatewayURL: "https://gateway.example/v1", E2BSandboxTimeout: 71 * time.Minute, E2BAllowInternet: true,
	}
	stored, created, err := store.CreateSandboxProfile(ctx, profile)
	if err != nil || !created || stored.BaseVerified() {
		t.Fatalf("created=%v profile=%#v err=%v", created, stored, err)
	}
	if _, err := store.SetDefaultSandboxProfile(ctx, name); err == nil {
		t.Fatal("unverified profile became the default")
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "pi 0.52.3")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	defaulted, err := store.SetDefaultSandboxProfile(ctx, name)
	if err != nil || !defaulted.Default || !defaulted.BaseVerified() {
		t.Fatalf("default profile=%#v err=%v", defaulted, err)
	}
	previousVerification := *defaulted.Verification
	refreshing, refreshedVerification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || refreshing.BaseVerified() || refreshedVerification.OwnershipNonce == previousVerification.OwnershipNonce || !refreshedVerification.AttemptedAt.After(previousVerification.AttemptedAt) {
		t.Fatalf("fresh verification profile=%#v receipt=%#v error=%v", refreshing, refreshedVerification, err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, refreshedVerification, "pi 0.52.4"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, refreshedVerification); err != nil {
		t.Fatal(err)
	}

	input := codingJobInput("profile-immutability-"+name, "bounded implementation", "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", "dorf/profile-immutability")
	input.SandboxProfile = name
	input.BaseBranch = "main"
	job, created, err := store.AdmitCoding(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	reverifying, activeVerification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || reverifying.BaseVerified() || activeVerification.OwnershipNonce == refreshedVerification.OwnershipNonce {
		t.Fatalf("active Job fresh verification profile=%#v receipt=%#v err=%v", reverifying, activeVerification, err)
	}
	replayed, created, err := store.AdmitCoding(ctx, input)
	if err != nil || created || replayed.ID != job.ID {
		t.Fatalf("existing admission replay during verification Job=%#v created=%v err=%v", replayed, created, err)
	}
	fenced := input
	fenced.AdmissionKey += "-during-reverify"
	fenced.Branch += "-during-reverify"
	if _, _, err := store.AdmitCoding(ctx, fenced); err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("new Job admitted through unsettled verification: %v", err)
	}
	verificationFailure := errors.New("transient verification failure")
	if err := store.RecordSandboxProfileVerificationError(ctx, activeVerification, verificationFailure); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, activeVerification); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AdmitCoding(ctx, fenced); err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("new Job admitted through failed verification: %v", err)
	}
	_, retryVerification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || retryVerification.OwnershipNonce != activeVerification.OwnershipNonce || !retryVerification.ProbeCompletedAt.IsZero() || !retryVerification.CleanedAt.IsZero() || retryVerification.LastError != "" {
		t.Fatalf("verification retry receipt=%#v err=%v", retryVerification, err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, retryVerification, "pi 0.52.5"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, retryVerification); err != nil {
		t.Fatal(err)
	}
	admittedAfterRetry, created, err := store.AdmitCoding(ctx, fenced)
	if err != nil || !created || admittedAfterRetry.SandboxProfile != name {
		t.Fatalf("admission after verification retry Job=%#v created=%v err=%v", admittedAfterRetry, created, err)
	}
	sameGateway := profile.E2BGatewayURL
	unchanged, updated, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{E2BGatewayURL: &sameGateway})
	if err != nil || updated || !unchanged.Default || !unchanged.BaseVerified() {
		t.Fatalf("no-op patch changed verified default profile: updated=%v profile=%#v err=%v", updated, unchanged, err)
	}
	changedGateway := "https://replacement.example/v1"
	if _, _, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{E2BGatewayURL: &changedGateway}); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("in-use profile update error=%v", err)
	}
	result, err := db.ExecContext(ctx, `update dorf.jobs set admission_open=false,cleanup_state='complete',cleaned_at=clock_timestamp() where id in ($1,$2)`, job.ID, admittedAfterRetry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 2 {
		t.Fatalf("complete profile Jobs rows=%d err=%v", rows, err)
	}
	changed, updated, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{E2BGatewayURL: &changedGateway})
	if err != nil || !updated {
		t.Fatal(err)
	}
	if changed.E2BGatewayURL != changedGateway || changed.Artifact != profile.Artifact || changed.Harness != profile.Harness ||
		changed.E2BSandboxTimeout != profile.E2BSandboxTimeout || changed.E2BAllowInternet != profile.E2BAllowInternet || changed.Default || changed.Verification != nil {
		t.Fatalf("patch changed omitted fields or retained verification: %#v", changed)
	}
	if !changed.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("patch changed profile creation time: got=%s want=%s", changed.CreatedAt, stored.CreatedAt)
	}
	repeated, created, err := store.AdmitCoding(ctx, input)
	if err != nil || created || repeated.ID != job.ID {
		t.Fatalf("completed Job idempotency depended on updated profile verification: Job=%#v created=%v err=%v", repeated, created, err)
	}
	unverified := input
	unverified.AdmissionKey += "-new"
	unverified.Branch += "-new"
	if _, _, err := store.AdmitCoding(ctx, unverified); err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("new Job admitted through updated unverified profile: %v", err)
	}
}

func TestSandboxProfileUpdateInvalidatesActiveVerification(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("verification-update-%d", time.Now().UnixNano())
	original := core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("b", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
	}
	if _, _, err := store.CreateSandboxProfile(ctx, original); err != nil {
		t.Fatal(err)
	}
	started, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || started.Artifact != original.Artifact {
		t.Fatalf("started profile=%#v err=%v", started, err)
	}
	updatedArtifact := strings.Repeat("c", 64)
	patch := postgres.SandboxProfilePatch{IncusArtifact: &updatedArtifact}
	if _, _, err := store.UpdateSandboxProfile(ctx, name, patch); err == nil || !strings.Contains(err.Error(), "verification Sandbox cleanup is incomplete") {
		t.Fatalf("active verification update error=%v", err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, verification); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.UpdateSandboxProfile(ctx, name, patch); err != nil || !changed {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, verification, "codex stale"); err == nil {
		t.Fatal("stale verification certified the updated profile definition")
	}
	stored, err := store.SandboxProfile(ctx, name)
	if err != nil || stored.Artifact != updatedArtifact || stored.Verification != nil || stored.BaseVerified() {
		t.Fatalf("updated profile=%#v err=%v", stored, err)
	}
}

func TestUnavailableSandboxProfileFencesNewJobsAndPreservesExactAttention(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("unavailable-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderE2B, Harness: "codex", Artifact: "dorf/missing:exact-build",
		E2BGatewayURL: "https://gateway.example/v1", E2BSandboxTimeout: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex 0.147.0")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	input := codingJobInput("profile-unavailable-"+name, "bounded implementation", strings.Repeat("a", 40), "dorf/profile-unavailable")
	input.SandboxProfile = name
	input.BaseBranch = "main"
	job, created, err := store.AdmitCoding(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	source := core.ScopedActionID(job.ID, core.ActionSandboxCreate, core.MainSandboxName(job.ID))
	failure := provider.ArtifactUnavailableErrorf("E2B template %q is unavailable", "dorf/missing:exact-build")
	if err := store.RecordSandboxProfileUnavailable(ctx, job.ID, name, source, failure); err != nil {
		t.Fatal(err)
	}
	stored, err := store.SandboxProfile(ctx, name)
	if err != nil || stored.BaseVerified() || stored.Verification == nil || stored.Verification.LastError != failure.Error() {
		t.Fatalf("unavailable profile=%#v err=%v", stored, err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, verification, "codex stale"); err == nil {
		t.Fatal("stale probe cleared the unavailable profile fence")
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, verification); err != nil {
		t.Fatalf("idempotent stale cleanup: %v", err)
	}
	stored, err = store.SandboxProfile(ctx, name)
	if err != nil || stored.BaseVerified() || stored.Verification == nil || stored.Verification.LastError != failure.Error() {
		t.Fatalf("stale receipt write reopened unavailable profile=%#v err=%v", stored, err)
	}
	stopped, err := store.Job(ctx, job.ID)
	if err != nil || stopped.WorkflowAttentionSource != source || stopped.WorkflowAttention != failure.Error() {
		t.Fatalf("stopped Job=%#v err=%v", stopped, err)
	}
	newInput := input
	newInput.AdmissionKey += "-new"
	newInput.Branch += "-new"
	if _, _, err := store.AdmitCoding(ctx, newInput); err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("new Job admitted through unavailable profile: %v", err)
	}
	cleaning := requestCleanupIntegration(t, core.Application{Store: store, Tasks: client}, job.ID)
	if cleaning.CleanupState != core.CleanupScheduled {
		t.Fatalf("cleanup from unavailable profile state=%q", cleaning.CleanupState)
	}
}

func TestSandboxProfileSchemaRejectsNullRequiredFacts(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	for _, statement := range []string{
		`insert into dorf.sandbox_profiles(name,provider,harness,artifact,incus_disk_size) values('invalid-incus-null','incus','codex',repeat('d',64),'40GiB')`,
		`insert into dorf.sandbox_profiles(name,provider,harness,artifact,e2b_sandbox_timeout_seconds,e2b_allow_internet) values('invalid-e2b-null','e2b','codex','dorf:build',3300,false)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted incomplete profile: %s", statement)
		}
	}
	name := fmt.Sprintf("invalid-verification-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("e", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		insert into dorf.sandbox_profile_verifications(
			profile_name,contract_version,sandbox_id,ownership_nonce,probe_completed_at
		) values($1,'base-1',$2,$3,clock_timestamp())`, name, "sandbox-"+name, strings.Repeat("f", 64)); err == nil {
		t.Fatal("schema accepted a completed profile probe without a Harness version")
	}
}

func TestExplicitSteerTargetsAndAcknowledgesExactActiveTurn(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "explicit-steer")
	active, err := codingDelivery(ctx, store, job.ID)
	if err != nil || active == nil {
		t.Fatalf("initial delivery=%#v err=%v", active, err)
	}
	if err := store.PrepareAgentRun(ctx, active.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	activeTurnID := "turn-active-" + job.ID
	if err := store.BindAgentRun(ctx, active.AgentRun.ID, "codex", threadID, activeTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	if candidate, err := codingDelivery(ctx, store, job.ID); err != nil || candidate == nil || candidate.Message.ID != active.Message.ID {
		t.Fatalf("active Turn reconciliation candidate=%#v err=%v", candidate, err)
	}

	steerInput := core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "operator-steer", Input: "correct the active work", Intent: core.MessageSteer}
	steer, created, err := store.AdmitCodingMessage(ctx, steerInput)
	if err != nil || !created || steer.Intent != core.MessageSteer || steer.TargetTurnID != activeTurnID {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	repeated, created, err := store.AdmitCodingMessage(ctx, steerInput)
	if err != nil || created || repeated != steer {
		t.Fatalf("idempotent steer=%#v created=%v err=%v", repeated, created, err)
	}
	changed := steerInput
	changed.Intent = core.MessageFollow
	if _, _, err := store.AdmitCodingMessage(ctx, changed); err == nil {
		t.Fatal("same caller identity accepted a changed delivery intent")
	}
	delivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || delivery == nil || delivery.Message.ID != steer.ID || delivery.AgentRun.ThreadID != threadID {
		t.Fatalf("steer delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", activeTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, delivery.AgentRun.ID, activeTurnID, "inProgress"); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 2 || deliveries[1].AgentRun.TurnID != activeTurnID || deliveries[1].Message.Intent != core.MessageSteer {
		t.Fatalf("steer deliveries=%#v err=%v", deliveries, err)
	}
	if err := store.BindAgentRun(ctx, active.AgentRun.ID, "codex", threadID, activeTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, delivery.AgentRun.ID, activeTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	repeated, created, err = store.AdmitCodingMessage(ctx, steerInput)
	if err != nil || created || repeated != steer {
		t.Fatalf("terminal-target replay retargeted or reauthorized: Message=%#v created=%v err=%v", repeated, created, err)
	}
	next, err := codingDelivery(ctx, store, job.ID)
	if err != nil || next != nil {
		t.Fatalf("delivery after steer=%#v err=%v, want active Turn observation", next, err)
	}
	other, _ := prepareTransportIntegrationJob(t, store, "steer-without-active-turn")
	if _, _, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: other.ID, SandboxID: core.MainSandboxName(other.ID), FromKind: "human", FromID: "invalid-steer", Input: "cannot target", Intent: core.MessageSteer}); err == nil || !strings.Contains(err.Error(), "exact active regular harness Turn") {
		t.Fatalf("steer without active turn error=%v", err)
	}
}

func TestCompletedSteerReceiptKeepsActiveTurnNonterminalAndWorkflowAttentionClear(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "active-steer-result")
	target, err := codingDelivery(ctx, store, job.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-active-" + job.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "inProgress"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{
		JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
		FromID: "active-steer", Input: "adjust the active Turn", Intent: core.MessageSteer,
	})
	if err != nil || !created || steer.TargetTurnID != targetTurnID {
		t.Fatalf("steer=%#v created=%t err=%v", steer, created, err)
	}
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, gitworkspace.ActionRepositoryClone, core.ActionRouteCreate} {
		action, err := store.GetOrCreateSandboxAction(ctx, core.MainSandboxName(job.ID), kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}

	externals := &integrationExternals{turns: []core.HarnessTurn{{ID: targetTurnID, Status: "inProgress"}}}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(resultBoundaryAgentExecution{store: store, externals: externals})
	resolver := integrationRuntimeResolver{execution: execution, profile: profileapp.Runtime{SandboxProfile: job.SandboxProfile}}
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver}
	service := coding.NewService(gitworkspace.NewExecutor(execution, externals, nil), store, externals, blob.Store{}, absurdruntime.RequireClaim)
	taskName := "dorf-active-steer-result-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		if err := execution.ReconcileJobAgent(taskCtx, job.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		handle, err := application.OpenJob(taskCtx, job.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		work, err := coding.RunJob(taskCtx, handle, service, store, coding.ProposalRuntime{}, job.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if work.Kind != coding.WorkWaitAgent || work.FactID != steer.ID {
			return core.TaskResultV1{}, fmt.Errorf("nonterminal steer work=%#v", work)
		}
		stored, err := store.Job(taskCtx, job.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if stored.WorkflowAttention != "" || stored.WorkflowAttentionSource != "" {
			return core.TaskResultV1{}, fmt.Errorf("nonterminal steer recorded workflow attention: %#v", stored)
		}
		externals.mu.Lock()
		externals.turns[0].Status = "completed"
		externals.turns[0].Output = "target completed"
		externals.mu.Unlock()
		if err := execution.ReconcileJobAgent(taskCtx, job.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		settled, err := execution.ObserveSettledAgentMessage(taskCtx, job.ID, target.Message.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if !settled.Terminal() || settled.Outcome != "completed" || settled.Output != "target completed" {
			return core.TaskResultV1{}, fmt.Errorf("active turn-start Message did not settle: %#v", settled)
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "active-steer-reconciled"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "active-steer-result", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}

	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 2 || deliveries[0].AgentRun.State != core.AgentRunCompleted || deliveries[1].AgentRun.State != core.AgentRunCompleted || deliveries[1].AgentRun.TurnOutcome != "completed" {
		t.Fatalf("settled active target and steer receipt=%#v err=%v", deliveries, err)
	}
}

func TestDeliveriesFailsLoudlyWhenMessageHasNoAgentRun(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _ := prepareTransportIntegrationJob(t, store, "orphan-message-read")
	orphanID := "message-orphan-" + job.ID
	if _, err := db.ExecContext(ctx, `
		insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input)
		values($1,$2,'human','corruption-test',2,'retained orphan input')`, orphanID, job.ID); err != nil {
		t.Fatal(err)
	}
	if deliveries, err := store.Deliveries(ctx, job.ID); err == nil || !strings.Contains(err.Error(), orphanID) {
		t.Fatalf("Deliveries=%#v error=%v, want named orphan Message failure", deliveries, err)
	}
}

func TestSharedSteersPersistEveryTerminalTargetOutcome(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			job, threadID := prepareTransportIntegrationJob(t, store, "shared-steer-outcome-"+status)
			target, err := codingDelivery(ctx, store, job.ID)
			if err != nil || target == nil {
				t.Fatalf("target delivery=%#v err=%v", target, err)
			}
			if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
				t.Fatal(err)
			}
			targetTurnID := "turn-shared-" + job.ID
			if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
				t.Fatal(err)
			}
			first, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "first-shared-steer", Input: "first accepted shared input", Intent: core.MessageSteer})
			if err != nil || !created {
				t.Fatalf("first steer=%#v created=%v err=%v", first, created, err)
			}
			second, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "second-shared-steer", Input: "second accepted shared input", Intent: core.MessageSteer})
			if err != nil || !created {
				t.Fatalf("second steer=%#v created=%v err=%v", second, created, err)
			}
			firstDelivery, err := codingDelivery(ctx, store, job.ID)
			if err != nil || firstDelivery == nil || firstDelivery.Message.ID != first.ID {
				t.Fatalf("first steer delivery=%#v err=%v", firstDelivery, err)
			}
			if err := store.PrepareAgentRun(ctx, firstDelivery.AgentRun.ID, "codex", targetTurnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindSteer(ctx, firstDelivery.AgentRun.ID, targetTurnID, "inProgress"); err != nil {
				t.Fatal(err)
			}
			secondDelivery, err := codingDelivery(ctx, store, job.ID)
			if err != nil || secondDelivery == nil || secondDelivery.Message.ID != second.ID {
				t.Fatalf("second steer delivery=%#v err=%v", secondDelivery, err)
			}
			if err := store.PrepareAgentRun(ctx, secondDelivery.AgentRun.ID, "codex", targetTurnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindSteer(ctx, secondDelivery.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindSteer(ctx, firstDelivery.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			deliveries, err := store.Deliveries(ctx, job.ID)
			if err != nil || len(deliveries) != 3 {
				t.Fatalf("deliveries=%#v err=%v", deliveries, err)
			}
			for index, delivery := range deliveries[1:] {
				if delivery.Message.Intent != core.MessageSteer || delivery.Message.TargetTurnID != targetTurnID || delivery.AgentRun.TurnID != targetTurnID || delivery.AgentRun.TurnOutcome != status || delivery.AgentRun.State != core.AgentRunCompleted {
					t.Fatalf("shared steer %d=%#v", index+1, delivery)
				}
			}
		})
	}
}

func TestSteerTerminalFallbackPreservesRequestAndSerializesLaterFIFO(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "steer-terminal-fallback")
	target, err := codingDelivery(ctx, store, job.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + job.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "terminal-race-steer", Input: "preserve exact durable input", Intent: core.MessageSteer})
	if err != nil || !created || steer.TargetTurnID != targetTurnID {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	fallback, err := codingDelivery(ctx, store, job.ID)
	if err != nil || fallback == nil || fallback.Message.ID != steer.ID || fallback.AgentRun.ID != core.AgentRunID(steer.ID) {
		t.Fatalf("fallback delivery=%#v err=%v", fallback, err)
	}
	if err := store.PrepareAgentRun(ctx, fallback.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}
	actualTurnID := "turn-fallback-" + job.ID
	if err := store.BindAgentRun(ctx, fallback.AgentRun.ID, "codex", threadID, actualTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	later, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "later-follow", Input: "later FIFO delivery"})
	if err != nil || !created {
		t.Fatalf("later=%#v created=%v err=%v", later, created, err)
	}
	active, err := codingDelivery(ctx, store, job.ID)
	if err != nil || active == nil || active.Message.ID != fallback.Message.ID {
		t.Fatalf("active fallback reconciliation=%#v err=%v", active, err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 3 {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	if deliveries[1].Message.Intent != core.MessageSteer || deliveries[1].Message.TargetTurnID != targetTurnID || deliveries[1].AgentRun.TurnID != actualTurnID {
		t.Fatalf("fallback delivery=%#v later=%#v", deliveries[1], deliveries[2])
	}
	if err := store.BindAgentRun(ctx, fallback.AgentRun.ID, "codex", threadID, actualTurnID, "failed"); err != nil {
		t.Fatal(err)
	}
	next, err := codingDelivery(ctx, store, job.ID)
	if err != nil || next == nil || next.Message.ID != later.ID || next.AgentRun.ThreadID != threadID {
		t.Fatalf("later delivery=%#v err=%v", next, err)
	}
	deliveries, err = store.Deliveries(ctx, job.ID)
	if err != nil || deliveries[1].AgentRun.State != core.AgentRunFailed || deliveries[1].AgentRun.TurnOutcome != "failed" || deliveries[1].Message.TargetTurnID != targetTurnID {
		t.Fatalf("terminal fallback evidence=%#v err=%v", deliveries[1], err)
	}
}

func TestTerminalHarnessTurnAllowsSameThreadFollowFIFO(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			job, threadID := prepareTransportIntegrationJob(t, store, "terminal-follow-"+status)
			first, err := codingDelivery(ctx, store, job.ID)
			if err != nil || first == nil {
				t.Fatalf("first=%#v err=%v", first, err)
			}
			if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", ""); err != nil {
				t.Fatal(err)
			}
			turnID := "turn-first-" + job.ID
			if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, turnID, "running"); err != nil {
				t.Fatal(err)
			}
			follow, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "queued-follow", Input: "continue after the accepted outcome"})
			if err != nil || !created || follow.Intent != core.MessageFollow {
				t.Fatalf("follow=%#v created=%v err=%v", follow, created, err)
			}
			stillActive, err := codingDelivery(ctx, store, job.ID)
			if err != nil || stillActive == nil || stillActive.Message.ID != first.Message.ID {
				t.Fatalf("delivery crossed active Turn: delivery=%#v err=%v", stillActive, err)
			}
			if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, turnID, status); err != nil {
				t.Fatal(err)
			}
			next, err := codingDelivery(ctx, store, job.ID)
			if err != nil || next == nil || next.Message.ID != follow.ID || next.AgentRun.ThreadID != threadID {
				t.Fatalf("follow after %s=%#v err=%v", status, next, err)
			}
			if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", turnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-follow-"+job.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			deliveries, err := store.Deliveries(ctx, job.ID)
			if err != nil || len(deliveries) != 2 || deliveries[0].AgentRun.TurnOutcome != status || deliveries[0].AgentRun.TurnID == "" || deliveries[1].AgentRun.State != core.AgentRunCompleted {
				t.Fatalf("preserved %s then follow=%#v err=%v", status, deliveries, err)
			}
		})
	}
}

func TestEarlyCodingFollowsAdoptAuthoritativeThreadAndSubmitDistinctTurns(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	job, created, err := store.AdmitCoding(ctx, codingJobInput(
		fmt.Sprintf("early-follow-%d", time.Now().UnixNano()),
		"execute every accepted early follow in FIFO order",
		strings.Repeat("e", 40),
		"dorf/early-follow",
	))
	if err != nil || !created {
		t.Fatalf("admit Job=%#v created=%t err=%v", job, created, err)
	}
	wantInputs := []string{job.Goal, "first early follow", "second early follow"}
	for i, input := range wantInputs[1:] {
		if _, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{
			JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
			FromID: fmt.Sprintf("early-follow-%d", i+1), Input: input,
		}); err != nil || !created {
			t.Fatalf("admit early follow %d created=%t err=%v", i+1, created, err)
		}
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != len(wantInputs) {
		t.Fatalf("early deliveries=%#v err=%v", deliveries, err)
	}
	messageIDs := make([]string, len(deliveries))
	for i, delivery := range deliveries {
		if delivery.Message.Sequence != int64(i+1) || delivery.AgentRun.ThreadID != "" || delivery.AgentRun.TurnID != "" {
			t.Fatalf("early delivery %d was prematurely bound: %#v", i, delivery)
		}
		messageIDs[i] = delivery.Message.ID
	}
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, gitworkspace.ActionRepositoryClone, core.ActionRouteCreate} {
		action, err := store.GetOrCreateSandboxAction(ctx, core.MainSandboxName(job.ID), kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}

	externals := &integrationExternals{turnStatus: "completed"}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(codingAgentExecution{store: store, externals: externals})
	taskName := "dorf-early-follow-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		for _, messageID := range messageIDs {
			if err := execution.ReconcileJobAgent(taskCtx, job.ID); err != nil {
				return core.TaskResultV1{}, err
			}
			result, err := execution.ObserveSettledAgentMessage(taskCtx, job.ID, messageID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			if !result.Terminal() || result.MessageID != messageID {
				return core.TaskResultV1{}, fmt.Errorf("Message %s did not reconcile terminally: %#v", messageID, result)
			}
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "early-follows-reconciled"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "early-follow-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}

	if got := externals.submittedSequences(); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("Harness submit order=%v", got)
	}
	if got := externals.submittedInputs(); !slices.Equal(got, wantInputs) {
		t.Fatalf("Harness inputs=%q want=%q", got, wantInputs)
	}
	deliveries, err = store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != len(wantInputs) {
		t.Fatalf("settled deliveries=%#v err=%v", deliveries, err)
	}
	threadID := deliveries[0].AgentRun.ThreadID
	turnIDs := make(map[string]struct{}, len(deliveries))
	for i, delivery := range deliveries {
		if delivery.AgentRun.State != core.AgentRunCompleted || threadID == "" || delivery.AgentRun.ThreadID != threadID || delivery.AgentRun.TurnID == "" {
			t.Fatalf("settled delivery %d did not share one authoritative Thread: %#v", i, delivery)
		}
		turnIDs[delivery.AgentRun.TurnID] = struct{}{}
	}
	if len(turnIDs) != len(deliveries) {
		t.Fatalf("early follows reused a prior Turn: %#v", deliveries)
	}
}

func TestEarlyCodingFollowNoThreadPredecessorRules(t *testing.T) {
	for _, test := range []struct {
		name        string
		settleFirst func(context.Context, postgres.Store, core.AgentRun) error
		wantFirst   bool
	}{
		{
			name: "definite failure allows next initial",
			settleFirst: func(ctx context.Context, store postgres.Store, run core.AgentRun) error {
				return store.FailAgentRun(ctx, run.ID, "definite no submit")
			},
		},
		{
			name: "submitting blocks later pending",
			settleFirst: func(ctx context.Context, store postgres.Store, run core.AgentRun) error {
				return store.PrepareAgentRun(ctx, run.ID, "codex", "")
			},
			wantFirst: true,
		},
		{
			name: "uncertain blocks later pending",
			settleFirst: func(ctx context.Context, store postgres.Store, run core.AgentRun) error {
				if err := store.PrepareAgentRun(ctx, run.ID, "codex", ""); err != nil {
					return err
				}
				return store.UncertainAgentRun(ctx, run.ID, "accepted effect visibility is ambiguous")
			},
			wantFirst: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			job, created, err := store.AdmitCoding(ctx, codingJobInput(
				fmt.Sprintf("early-no-thread-%s-%d", strings.ReplaceAll(test.name, " ", "-"), time.Now().UnixNano()),
				"initial work", strings.Repeat("f", 40), "dorf/early-no-thread",
			))
			if err != nil || !created {
				t.Fatalf("admit Job created=%t err=%v", created, err)
			}
			deliveries, err := store.Deliveries(ctx, job.ID)
			if err != nil || len(deliveries) != 1 {
				t.Fatalf("initial deliveries=%#v err=%v", deliveries, err)
			}
			first := deliveries[0]
			if err := test.settleFirst(ctx, store, first.AgentRun); err != nil {
				t.Fatal(err)
			}
			follow, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{
				JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
				FromID: "accepted-follow", Input: "continue after the predecessor",
			})
			if err != nil || !created {
				t.Fatalf("admit follow=%#v created=%t err=%v", follow, created, err)
			}
			selected, err := store.CodingAgentMessage(ctx, job.ID)
			if err != nil || selected == nil {
				t.Fatalf("selected=%#v err=%v", selected, err)
			}
			wantMessageID := follow.ID
			if test.wantFirst {
				wantMessageID = first.Message.ID
			}
			if selected.MessageID != wantMessageID {
				t.Fatalf("selected Message=%s want=%s", selected.MessageID, wantMessageID)
			}
			if !test.wantFirst {
				selectedRun, err := store.AgentMessageExecution(ctx, selected.MessageID)
				if err != nil || selectedRun.AgentRun.ThreadID != "" || selectedRun.AgentRun.Harness != "" || selectedRun.AgentRun.State != core.AgentRunPending {
					t.Fatalf("new initial execution=%#v err=%v", selectedRun, err)
				}
			}
		})
	}
}

func TestSubmittingFollowRemainsDeliveryCandidateUntilReconciled(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "submitting-follow-recovery")
	delivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || delivery == nil {
		t.Fatalf("initial delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || !slices.ContainsFunc(deliveries, func(candidate core.Delivery) bool {
		return candidate.AgentRun.ID == delivery.AgentRun.ID && candidate.AgentRun.BaselineRecorded && candidate.AgentRun.BaselineTurnID == ""
	}) {
		t.Fatalf("prepared Delivery baseline=%#v err=%v", deliveries, err)
	}
	later, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "later-follow", Input: "must wait for recovery"})
	if err != nil || !created {
		t.Fatalf("later Follow=%#v created=%v err=%v", later, created, err)
	}

	candidate, err := codingDelivery(ctx, store, job.ID)
	if err != nil || candidate == nil || candidate.AgentRun.ID != delivery.AgentRun.ID || candidate.AgentRun.State != core.AgentRunSubmitting {
		t.Fatalf("submitting candidate=%#v err=%v", candidate, err)
	}
	retry, err := codingDelivery(ctx, store, job.ID)
	if err != nil || retry == nil || retry.AgentRun.ID != delivery.AgentRun.ID || retry.AgentRun.State != core.AgentRunSubmitting {
		t.Fatalf("submitting retry=%#v err=%v", retry, err)
	}
	if err := store.BindAgentRun(ctx, retry.AgentRun.ID, "codex", threadID, "turn-recovered-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if next, err := codingDelivery(ctx, store, job.ID); err != nil || next == nil || next.Message.ID != later.ID {
		t.Fatalf("next candidate=%#v err=%v, want later Follow", next, err)
	}
}

func TestSubmittingSteerRemainsPriorityDeliveryUntilReconciled(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "submitting-steer-recovery")
	target, err := codingDelivery(ctx, store, job.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + job.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "recover-submitting-steer", Input: "adjust the active Turn", Intent: core.MessageSteer})
	if err != nil || !created {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	selected, err := codingDelivery(ctx, store, job.ID)
	if err != nil || selected == nil || selected.Message.ID != steer.ID {
		t.Fatalf("selected steer=%#v err=%v", selected, err)
	}
	if err := store.PrepareAgentRun(ctx, selected.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "queued-after-steer", Input: "run after the active Turn"}); err != nil || !created {
		t.Fatalf("queued Follow created=%v err=%v", created, err)
	}

	for _, load := range []struct {
		name string
		fn   func() (*core.Delivery, error)
	}{
		{name: "first reload", fn: func() (*core.Delivery, error) { return codingDelivery(ctx, store, job.ID) }},
		{name: "second reload", fn: func() (*core.Delivery, error) { return codingDelivery(ctx, store, job.ID) }},
	} {
		candidate, err := load.fn()
		if err != nil || candidate == nil || candidate.Message.ID != steer.ID || candidate.AgentRun.State != core.AgentRunSubmitting {
			t.Fatalf("%s submitting steer candidate=%#v err=%v", load.name, candidate, err)
		}
	}
}

func TestTerminalTargetSteerFallbackOwnsRevisionObservation(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "steer-fallback-revision")
	target, err := codingDelivery(ctx, store, job.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + job.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "terminal-target-fallback", Input: "continue in a new Turn", Intent: core.MessageSteer})
	if err != nil || !created {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	fallback, err := codingDelivery(ctx, store, job.ID)
	if err != nil || fallback == nil || fallback.Message.ID != steer.ID {
		t.Fatalf("fallback delivery=%#v err=%v", fallback, err)
	}
	if err := store.PrepareAgentRun(ctx, fallback.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, fallback.AgentRun.ID, "codex", threadID, "turn-fallback-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	evidence := integrationEvidence(fallback.AgentRun.ID, "git-revision", "", "", job.Revision, "9")
	evidence.AgentRunID = fallback.AgentRun.ID
	observation := gitworkspace.Observation{ComparisonBase: job.Revision, Revision: job.Revision, Tree: strings.Repeat("9", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}
	if err := store.RecordRevisionObservation(ctx, job.ID, fallback.AgentRun.ID, observation, evidence); err != nil {
		t.Fatalf("terminal-target fallback Revision observation: %v", err)
	}
}

func TestFailedAcceptedTurnRequiresLaterSuccessfulFollowBeforeRevisionObservation(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "failed-observation-gate")
	first, err := codingDelivery(ctx, store, job.ID)
	if err != nil || first == nil {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	firstTurnID := "turn-failed-" + job.ID
	if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, firstTurnID, "failed"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	observation := gitworkspace.Observation{ComparisonBase: job.Revision, Revision: job.Revision, Tree: strings.Repeat("8", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}
	failedEvidence := integrationEvidence(first.AgentRun.ID, "git-revision", "", "", job.Revision, "8")
	failedEvidence.AgentRunID = first.AgentRun.ID
	if err := store.RecordRevisionObservation(ctx, job.ID, first.AgentRun.ID, observation, failedEvidence); !errors.Is(err, postgres.ErrRevisionObservationSuperseded) {
		t.Fatalf("failed accepted turn crossed Revision observation gate: %v", err)
	}
	follow, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "successful-follow", Input: "finish the coding workflow"})
	if err != nil || !created {
		t.Fatalf("follow=%#v created=%v err=%v", follow, created, err)
	}
	completed := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-success-"+job.ID)
	recoveryEvidence := integrationEvidence(completed.ID, "git-revision", "", "", job.Revision, "7")
	recoveryEvidence.AgentRunID = completed.ID
	if err := store.RecordRevisionObservation(ctx, job.ID, completed.ID, observation, recoveryEvidence); err != nil {
		t.Fatalf("successful later Follow did not own Revision observation: %v", err)
	}
}

func prepareTransportIntegrationJob(t *testing.T, store postgres.Store, label string) (coding.Job, string) {
	t.Helper()
	ctx := context.Background()
	revision := strings.Repeat("a", 40)
	key := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	admitted, created, err := store.AdmitCoding(ctx, codingJobInput(key, "transport proof", revision, "dorf/"+label))
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", admitted, created, err)
	}
	job, err := store.CodingJob(ctx, admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	threadID := "thread-" + job.ID
	return job, threadID
}

func TestChangedAndUnchangedRevisionObservationsLinkExactImplementationAgentRuns(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	start, changed := strings.Repeat("1", 40), strings.Repeat("2", 40)
	input := codingJobInput(fmt.Sprintf("revision-evidence-%d", time.Now().UnixNano()), "bounded implementation", start, "dorf/revision-evidence")
	admitted, created, err := store.AdmitCoding(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", admitted, created, err)
	}
	job, err := store.CodingJob(ctx, admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(time.Microsecond)
	threadID := "thread-" + job.ID
	changedRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-changed-"+job.ID)
	changedEvidence := integrationEvidence(changedRun.ID, "git-revision", "", "", changed, "b")
	changedEvidence.AgentRunID = changedRun.ID
	changedObservation := gitworkspace.Observation{ComparisonBase: start, Revision: changed, Tree: strings.Repeat("4", 40), Branch: input.Branch, StartedAt: now, FinishedAt: now}
	if err := store.RecordRevisionObservation(ctx, job.ID, changedRun.ID, changedObservation, changedEvidence); err != nil {
		t.Fatalf("changed Revision observation: %v", err)
	}
	replayed, created, err := store.AdmitCoding(ctx, input)
	if err != nil || created || replayed.ID != job.ID {
		t.Fatalf("admission replay after Revision advance=%#v created=%v err=%v", replayed, created, err)
	}
	if err := store.RecordRevisionObservation(ctx, job.ID, changedRun.ID, changedObservation, changedEvidence); err != nil {
		t.Fatalf("replay exact changed Revision observation: %v", err)
	}

	follow, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman, FromID: "verify-unchanged", Input: "verify the current implementation"})
	if err != nil || !created || follow.JobID != job.ID {
		t.Fatalf("follow Message=%#v created=%v err=%v", follow, created, err)
	}
	unchangedRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-unchanged-"+job.ID)
	unchangedEvidence := integrationEvidence(unchangedRun.ID, "git-revision", "", "", changed, "d")
	unchangedEvidence.AgentRunID = unchangedRun.ID
	unchangedObservation := gitworkspace.Observation{ComparisonBase: changed, Revision: changed, Tree: strings.Repeat("4", 40), Branch: input.Branch, StartedAt: now, FinishedAt: now}
	if err := store.RecordRevisionObservation(ctx, job.ID, unchangedRun.ID, unchangedObservation, unchangedEvidence); err != nil {
		t.Fatalf("unchanged Revision observation: %v", err)
	}
	stored, err := store.CodingJob(ctx, job.ID)
	records, evidenceErr := store.Evidence(ctx, job.ID)
	if err != nil || evidenceErr != nil || stored.Revision != changed || changedRun.InputRevision != start || unchangedRun.InputRevision != changed || !slices.ContainsFunc(records, func(record core.Evidence) bool {
		return record.ID == changedEvidence.ID && record.AgentRunID == changedRun.ID && record.Revision == changed
	}) || !slices.ContainsFunc(records, func(record core.Evidence) bool {
		return record.ID == unchangedEvidence.ID && record.AgentRunID == unchangedRun.ID && record.Revision == changed
	}) {
		t.Fatalf("stored Job=%#v changedRun=%#v unchangedRun=%#v Evidence=%#v err=%v evidenceErr=%v", stored, changedRun, unchangedRun, records, err, evidenceErr)
	}
}

func reviewRunsForRevision(ctx context.Context, store postgres.Store, jobID, revision string) ([]coding.ReviewRunView, error) {
	_, runs, err := store.CodingMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(runs, func(run coding.ReviewRunView) bool {
		return run.InputRevision != revision
	}), nil
}

func reviewPlanForRevision(ctx context.Context, store postgres.Store, jobID, revision string) (coding.ReviewPlanRecord, error) {
	plans, err := store.ReviewPlans(ctx, jobID)
	if err != nil {
		return coding.ReviewPlanRecord{}, err
	}
	for _, plan := range plans {
		if plan.Revision == revision {
			return plan, nil
		}
	}
	return coding.ReviewPlanRecord{}, sql.ErrNoRows
}

func TestAtomicReviewPolicyPersistsNoReviewAndStableSelectedRuns(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	for _, test := range []struct {
		name  string
		paths []string
		roles []policy.Role
	}{
		{name: "explicit no review", paths: []string{"docs/review.md"}},
		{name: "mandatory selected role", paths: []string{"internal/auth/session.go"}, roles: []policy.Role{policy.RoleAuthAuthority}},
		{name: "multiple mandatory roles", paths: []string{"internal/auth/session.go", "web/login.tsx"}, roles: []policy.Role{policy.RoleAuthAuthority, policy.RoleBrowserUI}},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, revision, _ := prepareReviewIntegrationJob(t, store, test.name)
			facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, test.paths)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := policy.ReviewPolicy(facts)
			if err != nil {
				t.Fatal(err)
			}
			record := coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}
			if err := store.RecordReviewPolicy(ctx, record); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordReviewPolicy(ctx, record); err != nil {
				t.Fatalf("idempotent policy retry: %v", err)
			}
			persisted, err := reviewPlanForRevision(ctx, store, job.ID, revision)
			if err != nil || persisted.RecordedAt.IsZero() || persisted.Facts.Revision != revision || persisted.Plan.Decision != plan.Decision {
				t.Fatalf("persisted plan=%#v err=%v", persisted, err)
			}
			runs, err := reviewRunsForRevision(ctx, store, job.ID, revision)
			if len(test.roles) == 0 {
				if err != nil || len(runs) != 0 || persisted.Plan.Decision != "no-review" {
					t.Fatalf("no-review runs=%#v plan=%#v err=%v", runs, persisted, err)
				}
			} else {
				if err != nil || len(runs) != len(test.roles) {
					t.Fatalf("selected runs=%#v err=%v", runs, err)
				}
				for i, role := range test.roles {
					request := runs[i].Request
					wantInput := policy.RolePrompt(role, facts)
					if runs[i].Role != string(role) || runs[i].ID != core.AgentRunID(request.ID) || runs[i].MessageID != request.ID || runs[i].Capability != coding.ReviewReadOnlyCapability || runs[i].InputRevision != revision || request.ID != coding.ReviewRequestMessageID(job.ID, revision, string(role)) || request.JobID != job.ID || request.FromKind != core.MessageFromWorkflow || request.FromID != coding.ReviewRequestFromID(revision, string(role)) || request.Sequence != int64(i+2) || request.Input != wantInput || request.Intent != core.MessageFollow || request.TargetTurnID != "" || request.AdmittedAt.IsZero() || runs[i].SandboxID != runs[i].Sandbox.ID || runs[i].Sandbox.ID != coding.ReviewSandboxName(job.ID, runs[i].ID) || runs[i].Sandbox.JobID != job.ID || len(runs[i].Sandbox.OwnershipNonce) != 64 || len(runs[i].SubmissionNonce) != 64 {
						t.Fatalf("selected runs=%#v err=%v", runs, err)
					}
					for prior := 0; prior < i; prior++ {
						if runs[prior].Sandbox.ID == runs[i].Sandbox.ID || runs[prior].Sandbox.OwnershipNonce == runs[i].Sandbox.OwnershipNonce || runs[prior].SubmissionNonce == runs[i].SubmissionNonce {
							t.Fatalf("review Roles share an isolated resource identity: %#v", runs)
						}
					}
				}
			}
			changed := record
			changed.Plan.Decision = "invalid-retry-change"
			if err := store.RecordReviewPolicy(ctx, changed); err == nil || !strings.Contains(err.Error(), "changed across retry") {
				t.Fatalf("changed atomic policy error=%v", err)
			}
		})
	}
}

func TestUnsettledHarnessInventoryIncludesActiveReviewRegardlessOfRole(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, _ := prepareReviewIntegrationJob(t, store, "active-review-cleanup-inventory")
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"internal/auth/session.go"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(ctx, coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	runs, err := reviewRunsForRevision(ctx, store, job.ID, revision)
	if err != nil || len(runs) == 0 {
		t.Fatalf("review runs=%#v err=%v", runs, err)
	}
	run := runs[0]
	prepareReviewBoundaryResourcesIntegration(t, store, run)
	if err := store.PrepareAgentRun(ctx, run.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, run.ID, "codex", "review-thread-"+run.ID, "review-turn-"+run.ID, "running"); err != nil {
		t.Fatal(err)
	}
	unsettled, err := store.UnsettledAgentMessages(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range unsettled {
		if message.MessageID == run.MessageID && message.SandboxID == run.Sandbox.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("all-role unsettled inventory=%#v, want review Message %s", unsettled, run.MessageID)
	}
}

func TestReviewAgentOperationRequiresExactNamedSandboxForSubmitAndRecovery(t *testing.T) {
	for _, test := range []struct {
		name     string
		recovery bool
	}{
		{name: "submit"},
		{name: "recover accepted submit", recovery: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, client := testDatabase(t)
			ctx := context.Background()
			job, revision, _ := prepareReviewIntegrationJob(t, store, "named-sandbox-"+test.name)
			facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"internal/auth/session.go"})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := policy.ReviewPolicy(facts)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordReviewPolicy(ctx, coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}); err != nil {
				t.Fatal(err)
			}
			runs, err := reviewRunsForRevision(ctx, store, job.ID, revision)
			if err != nil || len(runs) != 1 {
				t.Fatalf("review runs=%#v err=%v", runs, err)
			}
			run := runs[0]
			prepareReviewBoundaryResourcesIntegration(t, store, run)
			if test.recovery {
				if err := store.PrepareAgentRun(ctx, run.ID, "codex", ""); err != nil {
					t.Fatal(err)
				}
			}

			externals := &reviewOperationIntegrationExternals{integrationExternals: &integrationExternals{}}
			agents := &reviewOperationIntegrationResolver{store: store, externals: externals, messageID: run.MessageID}
			execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).WithAgentExecution(agents)
			taskName := "dorf-review-named-sandbox-proof-v1"
			client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
				agents.sandboxID = core.MainSandboxName(job.ID)
				if err := execution.ReconcileJobAgent(taskCtx, job.ID); err == nil {
					return core.TaskResultV1{}, fmt.Errorf("default Sandbox satisfied an isolated review Message")
				}
				agents.sandboxID = run.Sandbox.ID
				if err := execution.ReconcileJobAgent(taskCtx, job.ID); err != nil {
					return core.TaskResultV1{}, err
				}
				result, err := execution.ObserveSettledAgentMessage(taskCtx, job.ID, run.MessageID)
				if err != nil {
					return core.TaskResultV1{}, err
				}
				if !result.Terminal() || result.Outcome != "completed" || result.Output == "" {
					return core.TaskResultV1{}, fmt.Errorf("review Message did not reconcile terminally: %#v", result)
				}
				return core.TaskResultV1{JobID: job.ID, Outcome: "review-reconciled"}, nil
			}))
			spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + job.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, taskName); err != nil {
				t.Fatal(err)
			}
			if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "review-named-sandbox", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
				t.Fatal(err)
			}

			submits, recoveries, sandboxes := externals.reviewCalls()
			if submits != boolInt(!test.recovery) || recoveries != boolInt(test.recovery) || !slices.Equal(sandboxes, []string{run.Sandbox.ID}) {
				t.Fatalf("review calls submit=%d recover=%d Sandboxes=%v", submits, recoveries, sandboxes)
			}
			settled, err := store.AgentMessageExecution(ctx, run.MessageID)
			if err != nil || settled.AgentRun.State != core.AgentRunCompleted || settled.Sandbox.ID != run.Sandbox.ID || settled.AgentRun.TurnID == "" {
				t.Fatalf("settled review=%#v err=%v", settled, err)
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestReviewPolicySerializesWithAdmissionClosureAndKeepsExactReplay(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	prepare := func(t *testing.T, suffix string) (coding.Job, coding.ReviewPlanRecord) {
		t.Helper()
		job, revision, _ := prepareReviewIntegrationJob(t, store, suffix)
		facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"docs/review.md"})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := policy.ReviewPolicy(facts)
		if err != nil {
			t.Fatal(err)
		}
		return job, coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}
	}

	t.Run("closed Job rejects only a new policy", func(t *testing.T) {
		job, record := prepare(t, "review-policy-closed")
		if err := store.RequestCleanup(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordReviewPolicy(ctx, record); err == nil || !strings.Contains(err.Error(), "cannot record a new review policy") {
			t.Fatalf("closed Job policy error=%v", err)
		}
		if _, err := reviewPlanForRevision(ctx, store, job.ID, record.Revision); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("closed Job persisted a new review policy: %v", err)
		}
	})

	t.Run("recorded policy remains an exact replay", func(t *testing.T) {
		job, record := prepare(t, "review-policy-replay-closed")
		if err := store.RecordReviewPolicy(ctx, record); err != nil {
			t.Fatal(err)
		}
		if err := store.RequestCleanup(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordReviewPolicy(ctx, record); err != nil {
			t.Fatalf("exact policy replay after closure: %v", err)
		}
	})

	t.Run("concurrent close leaves one truthful result", func(t *testing.T) {
		job, record := prepare(t, "review-policy-close-race")
		start := make(chan struct{})
		policyResult := make(chan error, 1)
		closeResult := make(chan error, 1)
		go func() {
			<-start
			policyResult <- store.RecordReviewPolicy(ctx, record)
		}()
		go func() {
			<-start
			closeResult <- store.RequestCleanup(ctx, job.ID)
		}()
		close(start)
		policyErr, closeErr := <-policyResult, <-closeResult
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		_, planErr := reviewPlanForRevision(ctx, store, job.ID, record.Revision)
		if policyErr == nil && planErr != nil {
			t.Fatalf("successful policy was not durable: %v", planErr)
		}
		if policyErr != nil && (!strings.Contains(policyErr.Error(), "cannot record a new review policy") || !errors.Is(planErr, sql.ErrNoRows)) {
			t.Fatalf("rejected policy=%v durable plan error=%v", policyErr, planErr)
		}
	})
}

func TestReviewerFeedbackBecomesIdempotentObservedImplementationMessage(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, _, implementationRun := prepareReviewFeedbackIntegration(t, store, "review-feedback-message")
	evidence := integrationEvidence(implementationRun.ID, "git-revision", "", "", revision, "9")
	evidence.AgentRunID = implementationRun.ID
	observation := gitworkspace.Observation{ComparisonBase: revision, Revision: revision, Tree: strings.Repeat("c", 40), Branch: job.Branch}
	if err := store.RecordRevisionObservation(ctx, job.ID, implementationRun.ID, observation, evidence); err != nil {
		t.Fatalf("unchanged review feedback observation: %v", err)
	}
	stored, err := store.CodingJob(ctx, job.ID)
	if err != nil || stored.Revision != revision {
		t.Fatalf("unchanged review feedback Job=%#v err=%v", stored, err)
	}
}

func TestReviewFeedbackReplaySurvivesClosureButNewFeedbackDoesNot(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, _ := prepareReviewIntegrationJob(t, store, "review-feedback-closed-admission")
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"internal/auth/session.go", "web/login.tsx"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(ctx, coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	runs, err := reviewRunsForRevision(ctx, store, job.ID, revision)
	if err != nil || len(runs) != 2 {
		t.Fatalf("review runs=%#v err=%v", runs, err)
	}
	feedback := func(run coding.ReviewRunView, digestByte string) (core.HarnessTurn, core.Evidence) {
		prepareReviewBoundaryIntegration(t, store, run)
		refreshed, err := store.ReviewRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		observed := integrationEvidence(refreshed.ID, "review-observation", "", "", revision, digestByte)
		observed.AgentRunID, observed.Producer = refreshed.ID, "dorf-agent-review"
		observed.StartedAt, observed.FinishedAt = refreshed.StartedAt, refreshed.FinishedAt
		return core.HarnessTurn{ID: refreshed.TurnID, Status: "completed", Output: "bounded feedback from " + refreshed.Role}, observed
	}
	firstOutcome, firstEvidence := feedback(runs[0], "6")
	first, created, err := store.RecordReviewFeedback(ctx, runs[0].ID, firstOutcome, firstEvidence)
	if err != nil || !created {
		t.Fatalf("first review feedback=%#v created=%t err=%v", first, created, err)
	}
	secondOutcome, secondEvidence := feedback(runs[1], "5")
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	replayed, created, err := store.RecordReviewFeedback(ctx, runs[0].ID, firstOutcome, firstEvidence)
	if err != nil || created || replayed != first {
		t.Fatalf("closed-Job feedback replay=%#v created=%t err=%v", replayed, created, err)
	}
	if _, _, err := store.RecordReviewFeedback(ctx, runs[1].ID, secondOutcome, secondEvidence); err == nil || !strings.Contains(err.Error(), "cannot accept new review feedback") {
		t.Fatalf("new feedback after closure error=%v", err)
	}
}

func prepareReviewFeedbackIntegration(t *testing.T, store postgres.Store, suffix string) (coding.Job, string, core.Message, core.AgentRun) {
	t.Helper()
	ctx := context.Background()
	job, revision, _ := prepareReviewIntegrationJob(t, store, suffix)
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"internal/auth/session.go"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	record := coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	runs, err := reviewRunsForRevision(ctx, store, job.ID, revision)
	if err != nil || len(runs) != 1 {
		t.Fatalf("review runs=%#v err=%v", runs, err)
	}
	reviewerRun := runs[0]
	request := reviewerRun.Request
	if reviewerRun.MessageID != request.ID || request.ID != coding.ReviewRequestMessageID(job.ID, revision, reviewerRun.Role) || request.FromID != coding.ReviewRequestFromID(revision, reviewerRun.Role) || request.Input == "" {
		t.Fatalf("review request Message -> producer chain run=%#v request=%#v", reviewerRun, request)
	}
	prepareReviewBoundaryIntegration(t, store, reviewerRun)
	reviewerRun, err = store.ReviewRun(ctx, reviewerRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback := "The authority path needs one bounded implementation adjustment."
	observed := integrationEvidence(reviewerRun.ID, "review-observation", "", "", revision, "7")
	observed.AgentRunID = reviewerRun.ID
	observed.Producer = "dorf-agent-review"
	observed.StartedAt, observed.FinishedAt = reviewerRun.StartedAt, reviewerRun.FinishedAt
	outcome := core.HarnessTurn{ID: "turn-" + reviewerRun.ID, Status: "completed", Output: feedback}
	if err := store.SetWorkflowAttention(ctx, job.ID, reviewerRun.ID, "review feedback is not yet durable"); err != nil {
		t.Fatal(err)
	}
	message, created, err := store.RecordReviewFeedback(ctx, reviewerRun.ID, outcome, observed)
	if err != nil || !created || message.Input != feedback || message.FromKind != "agent" || message.FromID != reviewerRun.ID || message.AdmittedAt.IsZero() {
		t.Fatalf("review feedback Message=%#v created=%t err=%v", message, created, err)
	}
	cleared, err := store.Job(ctx, job.ID)
	if err != nil || cleared.WorkflowAttention != "" || cleared.WorkflowAttentionSource != "" {
		t.Fatalf("durable review feedback left stale attention: Job=%#v err=%v", cleared, err)
	}
	if err := store.SetWorkflowAttention(ctx, job.ID, reviewerRun.ID, "review feedback replay is not yet reconciled"); err != nil {
		t.Fatal(err)
	}
	repeated, created, err := store.RecordReviewFeedback(ctx, reviewerRun.ID, outcome, observed)
	if err != nil || created || repeated != message {
		t.Fatalf("idempotent review feedback Message=%#v created=%t err=%v", repeated, created, err)
	}
	cleared, err = store.Job(ctx, job.ID)
	if err != nil || cleared.WorkflowAttention != "" || cleared.WorkflowAttentionSource != "" {
		t.Fatalf("idempotent review feedback left stale attention: Job=%#v err=%v", cleared, err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	requestIndex := slices.IndexFunc(deliveries, func(candidate core.Delivery) bool { return candidate.Message.ID == request.ID })
	feedbackIndex := slices.IndexFunc(deliveries, func(candidate core.Delivery) bool { return candidate.Message.ID == message.ID })
	if err != nil || requestIndex < 0 || feedbackIndex < 0 || deliveries[requestIndex].AgentRun.ID != reviewerRun.ID || deliveries[requestIndex].AgentRun.State != core.AgentRunCompleted || deliveries[feedbackIndex].AgentRun.ID != core.AgentRunID(message.ID) || deliveries[feedbackIndex].AgentRun.State != core.AgentRunPending {
		t.Fatalf("review request -> review AgentRun -> feedback Message chain=%#v err=%v", deliveries, err)
	}
	delivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || delivery == nil || delivery.Message != message || delivery.AgentRun.MessageID != message.ID || delivery.AgentRun.Role != "implement" {
		t.Fatalf("review feedback implementation delivery=%#v err=%v", delivery, err)
	}
	implementationRun := completeNextIntegrationRun(t, store, job.ID, "thread-"+job.ID, "turn-feedback-"+job.ID)
	if implementationRun.MessageID != message.ID || message.FromKind != core.MessageFromAgent || message.FromID != reviewerRun.ID {
		t.Fatalf("feedback Message -> implementation AgentRun chain message=%#v run=%#v", message, implementationRun)
	}
	return job, revision, message, implementationRun
}

func actionIntegrationJob(t *testing.T, suffix string) (*sql.DB, postgres.Store, core.Job) {
	t.Helper()
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	job, created, err := store.AdmitCoding(ctx, codingJobInput(
		fmt.Sprintf("action-%s-%d", suffix, time.Now().UnixNano()),
		"prove durable Action custody",
		strings.Repeat("a", 40),
		"dorf/action-integration",
	))
	if err != nil || !created {
		t.Fatalf("admit Job=%#v created=%t err=%v", job, created, err)
	}
	return db, store, job
}

func TestJobHandleEnsuresStableDefaultAndNamedSandboxes(t *testing.T) {
	db, store, job := actionIntegrationJob(t, "handle-sandbox-identity")
	ctx := context.Background()
	foreign, created, err := store.AdmitCoding(ctx, codingJobInput(
		fmt.Sprintf("foreign-sandbox-owner-%d", time.Now().UnixNano()), "own a conflicting Sandbox identity",
		strings.Repeat("b", 40), "dorf/foreign-sandbox-owner",
	))
	if err != nil || !created {
		t.Fatalf("admit foreign owner created=%t err=%v", created, err)
	}
	conflictName := "conflict"
	foreignNonce := fmt.Sprintf("%x", sha256.Sum256([]byte(job.ID+":"+conflictName)))
	if _, err := db.ExecContext(ctx, `insert into dorf.sandboxes(id,job_id,name,ownership_nonce) values($1,$2,$3,$4)`,
		core.NamedSandboxID(job.ID, conflictName), foreign.ID, "foreign", foreignNonce); err != nil {
		t.Fatal(err)
	}

	externals := &integrationExternals{}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
	profile := profileapp.Runtime{SandboxProfile: job.SandboxProfile}
	resolver := integrationRuntimeResolver{execution: execution, profile: profile}
	client := newFaultClient(t, store, "dorf-handle-sandbox-identity-"+job.ID)
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver}
	taskName := "dorf-handle-sandbox-identity-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		handle, err := application.OpenJob(taskCtx, job.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		for _, ensure := range []func(context.Context) (core.SandboxHandle, error){
			handle.EnsureDefaultSandbox,
			handle.EnsureDefaultSandbox,
			func(ctx context.Context) (core.SandboxHandle, error) { return handle.EnsureNamedSandbox(ctx, "review") },
			func(ctx context.Context) (core.SandboxHandle, error) { return handle.EnsureNamedSandbox(ctx, "review") },
		} {
			if _, err := ensure(taskCtx); err != nil {
				return core.TaskResultV1{}, err
			}
		}
		if _, err := handle.EnsureNamedSandbox(taskCtx, conflictName); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("foreign Sandbox identity was accepted")
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "sandboxes-ensured"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "handle-sandbox-identity", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{
		core.DefaultSandbox: core.NamedSandboxID(job.ID, core.DefaultSandbox),
		"review":            core.NamedSandboxID(job.ID, "review"),
	} {
		owned, err := store.Sandbox(ctx, id)
		if err != nil || owned.JobID != job.ID || owned.Name != name {
			t.Fatalf("Sandbox %q=%#v err=%v", name, owned, err)
		}
		action, err := store.GetOrCreateSandboxAction(ctx, id, core.ActionSandboxCreate)
		if err != nil || action.State != core.ActionSucceeded {
			t.Fatalf("Sandbox %q create Action=%#v err=%v", name, action, err)
		}
	}
	if effects := externals.effectKinds(); len(effects) != 2 || effects[0] != core.ActionSandboxCreate || effects[1] != core.ActionSandboxCreate {
		t.Fatalf("idempotent Sandbox effects=%v", effects)
	}
}

func TestSandboxCleanupRequiresRouteRevoke(t *testing.T) {
	_, store, job := actionIntegrationJob(t, "cleanup-order")
	ctx := context.Background()
	sandboxID := core.MainSandboxName(job.ID)
	externals := &integrationExternals{}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
	create, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	revoke, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionRouteRevoke)
	if err != nil {
		t.Fatal(err)
	}
	client := newFaultClient(t, store, "dorf-authority-"+job.ID)
	taskName := "dorf-authority-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		if err := service.ExecuteSandboxAction(taskCtx, "wrong-job", sandboxID, create.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("wrong Job selected a provider mutation")
		}
		if err := service.ExecuteSandboxAction(taskCtx, job.ID, "wrong-sandbox", create.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("wrong Sandbox selected a provider mutation")
		}
		if err := service.ExecuteSandboxAction(taskCtx, job.ID, sandboxID, revoke.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("route revoke reached provider before cleanup scheduling")
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "authority-refused"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "authority-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := externals.effectKinds(); len(got) != 0 {
		t.Fatalf("forged tuple or premature cleanup mutated provider: %v", got)
	}

	barrier := &failOnceWorkflowBarrier{point: core.BarrierSandboxCreated}
	recovery := core.NewExecutionService(store, externals, barrier, absurdruntime.RequireClaim)
	recoveryTaskName := "lost-provider-receipt-v1"
	client.MustRegister(absurd.Task(recoveryTaskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		if err := recovery.ExecuteSandboxAction(taskCtx, job.ID, sandboxID, create.Kind); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "provider-reconciled"}, nil
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))
	recoveryTask, err := client.Spawn(ctx, recoveryTaskName, core.JobTaskParams{JobID: job.ID, PreviousTaskID: spawned.TaskID}, absurd.SpawnOptions{IdempotencyKey: recoveryTaskName + ":" + job.ID, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, spawned.TaskID, recoveryTask.TaskID, recoveryTaskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "lost-provider-receipt", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	unsettled, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionSandboxCreate)
	if err != nil || unsettled.State != core.ActionUnsettled || len(externals.effectKinds()) != 1 {
		t.Fatalf("lost provider receipt action=%#v effects=%v err=%v", unsettled, externals.effectKinds(), err)
	}
	if _, err := (core.Application{Store: store, Tasks: client}).RetryFailedJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "lost-provider-receipt-retry", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	settled, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionSandboxCreate)
	if err != nil || settled.State != core.ActionSucceeded || len(externals.effectKinds()) != 2 {
		t.Fatalf("reconciled provider receipt action=%#v effects=%v err=%v", settled, externals.effectKinds(), err)
	}
}

func TestSandboxDeleteBeforeRevokeHasZeroProviderEffects(t *testing.T) {
	_, store, job := actionIntegrationJob(t, "delete-before-revoke")
	ctx := context.Background()
	owned, err := store.Sandbox(ctx, core.MainSandboxName(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	remove, err := store.GetOrCreateSandboxAction(ctx, owned.ID, core.ActionSandboxDelete)
	if err != nil {
		t.Fatal(err)
	}
	externals := &integrationExternals{}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
	client := newFaultClient(t, store, "dorf-delete-before-revoke-"+job.ID)
	client.MustRegister(absurd.Task(core.CleanupTaskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		cleaning, err := store.Job(taskCtx, job.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if err := service.ExecuteSandboxAction(taskCtx, cleaning.ID, owned.ID, remove.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("Sandbox delete reached provider before route revoke")
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "delete-refused"}, nil
	}))
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, core.CleanupTaskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: "delete-before-revoke:" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, job.ID, "", spawned.TaskID, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "delete-before-revoke", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := externals.effectKinds(); len(got) != 0 {
		t.Fatalf("delete-before-revoke provider effects=%v", got)
	}
	unsettled, err := store.GetOrCreateSandboxAction(ctx, owned.ID, core.ActionSandboxDelete)
	if err != nil || unsettled.State != core.ActionUnsettled {
		t.Fatalf("delete-before-revoke Action=%#v err=%v", unsettled, err)
	}
}

func TestCleanupOnlyObservesAcceptedSteerAndBlocksDestructiveActions(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "cleanup-accepted-steer")
	target, err := codingDelivery(ctx, store, job.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-cleanup-steer-" + job.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{
		JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
		FromID: "cleanup-accepted-steer", Input: "accepted before cleanup", Intent: core.MessageSteer,
	})
	if err != nil || !created {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	steerDelivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || steerDelivery == nil || steerDelivery.Message.ID != steer.ID {
		t.Fatalf("steer delivery=%#v err=%v", steerDelivery, err)
	}
	if err := store.PrepareAgentRun(ctx, steerDelivery.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}

	sandboxID := core.MainSandboxName(job.ID)
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, core.ActionRouteCreate} {
		action, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	revoke, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionRouteRevoke)
	if err != nil {
		t.Fatal(err)
	}
	externals := &integrationExternals{turns: []core.HarnessTurn{{
		ID: targetTurnID, Status: "inProgress", AcceptedMessageIDs: []string{steerDelivery.AgentRun.ID},
	}}}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(&cleanupOnlyAgentExecution{externals: externals})
	client := newFaultClient(t, store, "dorf-cleanup-accepted-steer-"+job.ID)
	client.MustRegister(absurd.Task(core.CleanupTaskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		if _, _, err := service.PrepareCleanup(taskCtx, job.ID); err == nil || !strings.Contains(err.Error(), "remain") {
			return core.TaskResultV1{}, fmt.Errorf("cleanup did not retain active accepted steer: %v", err)
		}
		if err := service.ExecuteSandboxAction(taskCtx, job.ID, sandboxID, revoke.Kind); err == nil || !strings.Contains(err.Error(), "Harness mutations remain unsettled") {
			return core.TaskResultV1{}, fmt.Errorf("route revoke did not enforce Harness barrier: %v", err)
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "accepted-steer-retained"}, nil
	}))
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, core.CleanupTaskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: "cleanup-accepted-steer:" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, job.ID, job.CurrentTaskID, spawned.TaskID, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "cleanup-accepted-steer", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if submitted := externals.submittedSequences(); len(submitted) != 0 {
		t.Fatalf("cleanup submitted or steered Harness work: %v", submitted)
	}
	if effects := externals.effectKinds(); len(effects) != 0 {
		t.Fatalf("cleanup performed destructive effects: %v", effects)
	}
	unsettled, err := store.UnsettledAgentMessages(ctx, job.ID)
	if err != nil || len(unsettled) != 1 || unsettled[0].MessageID != target.Message.ID {
		t.Fatalf("retained active target=%#v err=%v", unsettled, err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var settledSteer core.AgentRun
	for _, delivery := range deliveries {
		if delivery.Message.ID == steer.ID {
			settledSteer = delivery.AgentRun
		}
	}
	if settledSteer.State != core.AgentRunCompleted || settledSteer.TurnID != targetTurnID {
		t.Fatalf("accepted steer was not settled from observation: %#v", settledSteer)
	}
}

func TestClosedAdmissionCleanupRecoversOrdinaryCodingRunWithoutExecutionEligibility(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "cleanup-ordinary-closed-admission")
	delivery, err := codingDelivery(ctx, store, job.ID)
	if err != nil || delivery == nil {
		t.Fatalf("ordinary delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	turnID := "turn-cleanup-ordinary-" + job.ID
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", threadID, turnID, "running"); err != nil {
		t.Fatal(err)
	}

	sandboxID := core.MainSandboxName(job.ID)
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, core.ActionRouteCreate} {
		action, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	revoke, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionRouteRevoke)
	if err != nil {
		t.Fatal(err)
	}
	remove, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionSandboxDelete)
	if err != nil {
		t.Fatal(err)
	}
	externals := &integrationExternals{turns: []core.HarnessTurn{{ID: turnID, Status: "completed"}}}
	agents := &cleanupOnlyAgentExecution{externals: externals}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).WithAgentExecution(agents)
	client := newFaultClient(t, store, "dorf-cleanup-ordinary-closed-"+job.ID)
	client.MustRegister(absurd.Task(core.CleanupTaskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		cleaning, sandboxes, err := service.PrepareCleanup(taskCtx, job.ID)
		if err != nil || cleaning.AdmissionOpen || len(sandboxes) != 1 {
			return core.TaskResultV1{}, fmt.Errorf("prepare closed ordinary cleanup: Job=%#v Sandboxes=%#v: %w", cleaning, sandboxes, err)
		}
		if err := service.ExecuteSandboxAction(taskCtx, job.ID, sandboxID, revoke.Kind); err != nil {
			return core.TaskResultV1{}, err
		}
		if err := service.ExecuteSandboxAction(taskCtx, job.ID, sandboxID, remove.Kind); err != nil {
			return core.TaskResultV1{}, err
		}
		if err := service.CompleteCleanup(taskCtx, job.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "ordinary-cleanup-complete"}, nil
	}))
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, core.CleanupTaskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: "cleanup-ordinary-closed:" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, job.ID, job.CurrentTaskID, spawned.TaskID, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "cleanup-ordinary-closed", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("cleanup Job=%#v err=%v", cleaned, err)
	}
	if agents.executionCalls != 0 || agents.cleanupCalls != 1 {
		t.Fatalf("Agent resolver calls: execution=%d cleanup=%d", agents.executionCalls, agents.cleanupCalls)
	}
	if submitted := externals.submittedSequences(); len(submitted) != 0 {
		t.Fatalf("cleanup submitted or steered ordinary Harness work: %v", submitted)
	}
	if effects := externals.effectKinds(); fmt.Sprint(effects) != fmt.Sprint([]core.ActionKind{core.ActionRouteRevoke, core.ActionSandboxDelete}) {
		t.Fatalf("cleanup effects=%v", effects)
	}
}

func TestActionKindGrammar(t *testing.T) {
	db, _, job := actionIntegrationJob(t, "kind-grammar")
	ctx := context.Background()
	for i, test := range []struct {
		kind  string
		valid bool
	}{
		{kind: "a", valid: true},
		{kind: "step-2", valid: true},
		{kind: strings.Repeat("a", 63), valid: true},
		{kind: ""},
		{kind: "2-step"},
		{kind: "Uppercase"},
		{kind: "under_score"},
		{kind: strings.Repeat("a", 64)},
	} {
		_, err := db.ExecContext(ctx, `
			insert into dorf.actions(id,job_id,kind,state,scope_key)
			values($1,$2,$3,'unsettled',$4)`, fmt.Sprintf("action-kind-%d-%s", i, job.ID), job.ID, test.kind, fmt.Sprintf("scope-%d", i))
		if accepted := err == nil; accepted != test.valid {
			t.Errorf("Action kind %q accepted=%t, want %t: %v", test.kind, accepted, test.valid, err)
		}
	}
}

func TestWorkflowOwnedSandboxActionCreationAndSettlement(t *testing.T) {
	_, store, job := actionIntegrationJob(t, "workflow-owned")
	ctx := context.Background()
	sandboxID := core.MainSandboxName(job.ID)
	kind := core.ActionKind("workflow-owned-sandbox-step")
	action, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
	if err != nil {
		t.Fatal(err)
	}
	wantID := core.ScopedActionID(job.ID, kind, sandboxID)
	if action.ID != wantID || action.JobID != job.ID || action.Kind != kind || action.Scope != sandboxID || action.State != core.ActionUnsettled {
		t.Fatalf("created workflow Action=%#v, want exact ID=%s Job=%s kind=%s Sandbox=%s", action, wantID, job.ID, kind, sandboxID)
	}
	if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
		t.Fatal(err)
	}
	settled, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
	if err != nil || settled.State != core.ActionSucceeded || settled.SettledAt.IsZero() {
		t.Fatalf("settled workflow Action=%#v err=%v", settled, err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
		t.Fatalf("immutable settlement replay: %v", err)
	}
	replayed, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
	if err != nil || replayed != settled {
		t.Fatalf("workflow Action replay=%#v, want immutable %#v, err=%v", replayed, settled, err)
	}
}

func prepareReviewBoundaryIntegration(t *testing.T, store postgres.Store, run coding.ReviewRunView) {
	t.Helper()
	ctx := context.Background()
	prepareReviewBoundaryResourcesIntegration(t, store, run)
	if err := store.PrepareAgentRun(ctx, run.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	threadID := "review-thread-" + run.ID
	if err := store.BindAgentRun(ctx, run.ID, "codex", threadID, "turn-"+run.ID, "completed"); err != nil {
		t.Fatal(err)
	}
}

func prepareReviewBoundaryResourcesIntegration(t *testing.T, store postgres.Store, run coding.ReviewRunView) {
	t.Helper()
	ctx := context.Background()
	sandbox, err := store.GetOrCreateSandboxAction(ctx, run.Sandbox.ID, core.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.State != core.ActionSucceeded {
		if err := store.RecordSandboxActionSuccess(ctx, sandbox.ID); err != nil {
			t.Fatal(err)
		}
	}
	checkout, err := store.GetOrCreateSandboxAction(ctx, run.Sandbox.ID, coding.ActionReviewCheckout)
	if err != nil {
		t.Fatal(err)
	}
	if checkout.State != core.ActionSucceeded {
		if err := store.RecordSandboxActionSuccess(ctx, checkout.ID); err != nil {
			t.Fatal(err)
		}
	}
	route, err := store.GetOrCreateSandboxAction(ctx, run.Sandbox.ID, core.ActionRouteCreate)
	if err != nil {
		t.Fatal(err)
	}
	if route.State != core.ActionSucceeded {
		if err := store.RecordSandboxActionSuccess(ctx, route.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func prepareReviewIntegrationJob(t *testing.T, store postgres.Store, suffix string) (coding.Job, string, string) {
	t.Helper()
	ctx := context.Background()
	start, revision := strings.Repeat("a", 40), strings.Repeat("b", 40)
	admitted, created, err := store.AdmitCoding(ctx, codingJobInput(fmt.Sprintf("review-policy-%s-%d", strings.ReplaceAll(suffix, " ", "-"), time.Now().UnixNano()), "bounded implementation", start, "dorf/review-policy"))
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", admitted, created, err)
	}
	job, err := store.CodingJob(ctx, admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	implementationRun := completeNextIntegrationRun(t, store, job.ID, "thread-"+job.ID, "turn-"+job.ID)
	evidence := integrationEvidence(implementationRun.ID, "git-revision", "", "", revision, "2")
	evidence.AgentRunID = implementationRun.ID
	if err := store.RecordRevisionObservation(ctx, job.ID, implementationRun.ID, gitworkspace.Observation{ComparisonBase: start, Revision: revision, Tree: strings.Repeat("c", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}, evidence); err != nil {
		t.Fatalf("Revision observation: %v", err)
	}
	job, err = store.CodingJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job, revision, ""
}

func TestRevisionObservationBoundaryIncludesLateSteeringAtomically(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	start := strings.Repeat("6", 40)
	revision := strings.Repeat("7", 40)
	branch := "dorf/revision-observation-boundary"
	key := fmt.Sprintf("revision-observation-boundary-%d", time.Now().UnixNano())
	job, created, err := store.AdmitCoding(ctx, codingJobInput(key, "bounded implementation", start, branch))
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	threadID := "thread-" + job.ID
	initialRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-initial-"+job.ID)
	if delivery, err := codingDelivery(ctx, store, job.ID); err != nil || delivery != nil {
		t.Fatalf("pre-boundary delivery=%#v err=%v", delivery, err)
	}

	late, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "late-before-observation", Input: "include this bounded steering"})
	if err != nil || !created {
		t.Fatalf("late admission=%#v created=%v err=%v", late, created, err)
	}
	observation := gitworkspace.Observation{ComparisonBase: start, Revision: revision, Tree: strings.Repeat("8", 40), Branch: branch, StartedAt: now, FinishedAt: now}
	initialEvidence := integrationEvidence(initialRun.ID, "git-revision", "", "", revision, "9")
	initialEvidence.AgentRunID = initialRun.ID
	if err := store.RecordRevisionObservation(ctx, job.ID, initialRun.ID, observation, initialEvidence); !errors.Is(err, postgres.ErrRevisionObservationSuperseded) {
		t.Fatalf("Revision observation crossed admitted FIFO: %v", err)
	}
	includedRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-late-"+job.ID)
	evidence := integrationEvidence(includedRun.ID, "git-revision", "", "", revision, "a")
	evidence.AgentRunID = includedRun.ID
	afterCandidate, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "late-after-candidate", Input: "include the message admitted during Git inspection"})
	if err != nil || !created {
		t.Fatalf("late post-candidate admission=%#v created=%v err=%v", afterCandidate, created, err)
	}
	if err := store.RecordRevisionObservation(ctx, job.ID, includedRun.ID, observation, evidence); !errors.Is(err, postgres.ErrRevisionObservationSuperseded) {
		t.Fatalf("Revision observation skipped late accepted input: %v", err)
	}
	finalRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-after-candidate-"+job.ID)
	finalEvidence := integrationEvidence(finalRun.ID, "git-revision", "", "", revision, "b")
	finalEvidence.AgentRunID = finalRun.ID
	if err := store.RecordRevisionObservation(ctx, job.ID, finalRun.ID, observation, finalEvidence); err != nil {
		t.Fatalf("final Revision observation: %v", err)
	}
	retry, created, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: late.FromKind, FromID: late.FromID, Input: late.Input})
	if err != nil || created || retry != late {
		t.Fatalf("idempotent admitted retry=%#v created=%v err=%v", retry, created, err)
	}
}

func integrationEvidence(owner, kind, actionID, _ string, revision, digestByte string) core.Evidence {
	now := time.Now().UTC().Round(time.Microsecond)
	return core.Evidence{ID: core.EvidenceID(owner, kind), Digest: strings.Repeat(digestByte, 64), ByteSize: 10, MediaType: "application/vnd.dorf.observation+json", Producer: "integration-test", Kind: kind, ActionID: actionID, Revision: revision, StartedAt: now, FinishedAt: now}
}

func completeNextIntegrationRun(t *testing.T, store postgres.Store, jobID, threadID, turnID string) core.AgentRun {
	t.Helper()
	delivery, err := codingDelivery(context.Background(), store, jobID)
	if err != nil || delivery == nil {
		t.Fatalf("next delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(context.Background(), delivery.AgentRun.ID, "codex", delivery.AgentRun.BaselineTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(context.Background(), delivery.AgentRun.ID, "codex", threadID, turnID, "completed"); err != nil {
		t.Fatal(err)
	}
	return delivery.AgentRun
}

type integrationExternals struct {
	coding.ReviewExecution
	gitworkspace.Operations
	mu         sync.Mutex
	turns      []core.HarnessTurn
	submitted  []int64
	inputs     []string
	effects    []core.ActionKind
	turnStatus string
}

type reviewOperationIntegrationExternals struct {
	*integrationExternals
	reviewMu   sync.Mutex
	submits    int
	recoveries int
	sandboxes  []string
}

func (e *reviewOperationIntegrationExternals) ReviewInitialTurn(_ context.Context, _ coding.Job, run coding.ReviewRunView) (core.HarnessBinding, error) {
	e.reviewMu.Lock()
	e.submits++
	e.sandboxes = append(e.sandboxes, run.Sandbox.ID)
	e.reviewMu.Unlock()
	return reviewIntegrationBinding(run), nil
}

func (e *reviewOperationIntegrationExternals) ReviewRecover(_ context.Context, _ coding.Job, run coding.ReviewRunView) (core.HarnessBinding, error) {
	e.reviewMu.Lock()
	e.recoveries++
	e.sandboxes = append(e.sandboxes, run.Sandbox.ID)
	e.reviewMu.Unlock()
	return reviewIntegrationBinding(run), nil
}

func (*reviewOperationIntegrationExternals) ReviewTurns(_ context.Context, _ coding.Job, run coding.ReviewRunView) (core.HarnessHistory, error) {
	binding := reviewIntegrationBinding(run)
	return core.HarnessHistory{Harness: binding.Harness, ThreadID: binding.ThreadID, Turns: []core.HarnessTurn{binding.Turn}}, nil
}

func reviewIntegrationBinding(run coding.ReviewRunView) core.HarnessBinding {
	return core.HarnessBinding{
		Harness: "codex", ThreadID: "review-thread-" + run.ID,
		Turn: core.HarnessTurn{ID: "review-turn-" + run.ID, Status: "completed", Output: "strict review feedback"},
	}
}

func (e *reviewOperationIntegrationExternals) reviewCalls() (int, int, []string) {
	e.reviewMu.Lock()
	defer e.reviewMu.Unlock()
	return e.submits, e.recoveries, append([]string(nil), e.sandboxes...)
}

type reviewOperationIntegrationResolver struct {
	store     postgres.Store
	externals *reviewOperationIntegrationExternals
	messageID string
	sandboxID string
}

func (r reviewOperationIntegrationResolver) SelectAgentMessage(context.Context, string) (*core.AgentMessageWork, error) {
	return &core.AgentMessageWork{MessageID: r.messageID, SandboxID: r.sandboxID}, nil
}

func (reviewOperationIntegrationResolver) ResolveAgentPrompt(_ context.Context, execution core.AgentMessageExecution) (string, error) {
	return execution.Message.Input, nil
}

func (r reviewOperationIntegrationResolver) ResolveAgentRunOperation(ctx context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	return coding.NewReviewAgentOperation(ctx, r.store, r.externals, execution)
}

type codingAgentExecution struct {
	store     postgres.Store
	externals *integrationExternals
}

type countingCodingAgentExecution struct {
	codingAgentExecution
	selections int
}

func (s *countingCodingAgentExecution) SelectAgentMessage(ctx context.Context, jobID string) (*core.AgentMessageWork, error) {
	s.selections++
	return s.codingAgentExecution.SelectAgentMessage(ctx, jobID)
}

func (s codingAgentExecution) SelectAgentMessage(ctx context.Context, jobID string) (*core.AgentMessageWork, error) {
	return coding.SelectAgentMessage(ctx, s.store, jobID)
}

func (s codingAgentExecution) ResolveAgentPrompt(ctx context.Context, execution core.AgentMessageExecution) (string, error) {
	if err := s.store.ValidateCodingAgentMessage(ctx, execution); err != nil {
		return "", err
	}
	return execution.Message.Input, nil
}

func (s codingAgentExecution) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	return integrationAgentOperation{externals: s.externals, execution: execution}, nil
}

// resultBoundaryAgentExecution leaves selector eligibility to the real
// coding coordinator so this proof can replay a completed steer receipt and
// exercise both Core completed-run result branches.
type resultBoundaryAgentExecution struct {
	store     postgres.Store
	fixed     *core.AgentMessageWork
	externals *integrationExternals
	operation core.AgentRunOperation
}

func (s resultBoundaryAgentExecution) SelectAgentMessage(ctx context.Context, jobID string) (*core.AgentMessageWork, error) {
	if s.fixed != nil {
		selected := *s.fixed
		return &selected, nil
	}
	return s.store.CodingAgentMessage(ctx, jobID)
}

func (resultBoundaryAgentExecution) ResolveAgentPrompt(_ context.Context, execution core.AgentMessageExecution) (string, error) {
	return execution.Message.Input, nil
}

func (s resultBoundaryAgentExecution) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	if s.operation != nil {
		return s.operation, nil
	}
	return integrationAgentOperation{externals: s.externals, execution: execution}, nil
}

type cleanupOnlyAgentExecution struct {
	executionCalls int
	cleanupCalls   int
	externals      *integrationExternals
}

func (s *cleanupOnlyAgentExecution) SelectAgentMessage(context.Context, string) (*core.AgentMessageWork, error) {
	s.executionCalls++
	return nil, errors.New("ordinary execution selection must not run after admission closes")
}

func (s *cleanupOnlyAgentExecution) ResolveAgentPrompt(context.Context, core.AgentMessageExecution) (string, error) {
	s.executionCalls++
	return "", errors.New("ordinary execution eligibility must not run after admission closes")
}

func (s *cleanupOnlyAgentExecution) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	if execution.Job.AdmissionOpen {
		s.executionCalls++
		return nil, errors.New("ordinary execution Harness selection unexpectedly ran")
	}
	s.cleanupCalls++
	if execution.AgentRun.Role != "implement" {
		return nil, fmt.Errorf("cleanup did not reload the exact closed ordinary coding run")
	}
	return integrationAgentOperation{externals: s.externals, execution: execution}, nil
}

func (*integrationExternals) Harness() string { return "codex" }

type integrationAgentOperation struct {
	externals *integrationExternals
	execution core.AgentMessageExecution
}

func (o integrationAgentOperation) Harness() string { return "codex" }
func (o integrationAgentOperation) Submit(_ context.Context, run core.AgentRun, input string) (core.HarnessBinding, error) {
	o.externals.mu.Lock()
	defer o.externals.mu.Unlock()
	status := o.externals.turnStatus
	if status == "" {
		status = "running"
	}
	if run.ThreadID == "" {
		if len(o.externals.turns) == 0 {
			turn := core.HarnessTurn{ID: "integration-turn-" + o.execution.Message.ID, Status: status}
			o.externals.submitted = append(o.externals.submitted, o.execution.Message.Sequence)
			o.externals.inputs = append(o.externals.inputs, input)
			o.externals.turns = append(o.externals.turns, turn)
		}
		return core.HarnessBinding{Harness: "codex", ThreadID: "integration-thread-" + o.execution.Job.ID, Turn: o.externals.turns[0]}, nil
	}
	turn := core.HarnessTurn{ID: "integration-turn-" + o.execution.Message.ID, Status: status}
	o.externals.submitted = append(o.externals.submitted, o.execution.Message.Sequence)
	o.externals.inputs = append(o.externals.inputs, input)
	o.externals.turns = append(o.externals.turns, turn)
	return core.HarnessBinding{Harness: "codex", ThreadID: run.ThreadID, Turn: turn}, nil
}
func (o integrationAgentOperation) Recover(_ context.Context, _ core.AgentRun) (core.HarnessBinding, error) {
	o.externals.mu.Lock()
	defer o.externals.mu.Unlock()
	if len(o.externals.turns) == 0 {
		return core.HarnessBinding{}, nil
	}
	return core.HarnessBinding{Harness: "codex", ThreadID: "integration-thread-" + o.execution.Job.ID, Turn: o.externals.turns[len(o.externals.turns)-1]}, nil
}
func (o integrationAgentOperation) History(_ context.Context, run core.AgentRun) (core.HarnessHistory, error) {
	o.externals.mu.Lock()
	defer o.externals.mu.Unlock()
	threadID := run.ThreadID
	if threadID == "" {
		threadID = "integration-thread-" + o.execution.Job.ID
	}
	return core.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: append([]core.HarnessTurn(nil), o.externals.turns...)}, nil
}

func (e *integrationExternals) effect(kind core.ActionKind) error {
	e.mu.Lock()
	e.effects = append(e.effects, kind)
	e.mu.Unlock()
	return nil
}
func (e *integrationExternals) SandboxCreate(context.Context, core.Job, core.Sandbox) error {
	return e.effect(core.ActionSandboxCreate)
}
func (e *integrationExternals) ReconcileClone(context.Context, provider.Ownership, string, string, string) error {
	return e.effect(gitworkspace.ActionRepositoryClone)
}
func (e *integrationExternals) Reconcile(context.Context, core.Job, core.Sandbox, investigation.Source, []byte) error {
	return e.effect(investigation.ActionRepositoryRestore)
}
func (e *integrationExternals) RouteCreate(context.Context, core.Job, core.Sandbox, core.Route) error {
	return e.effect(core.ActionRouteCreate)
}
func (e *integrationExternals) SteerHistory(_ context.Context, _ core.Job, _ string, threadID string) (core.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return core.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: append([]core.HarnessTurn(nil), e.turns...)}, nil
}
func (e *integrationExternals) AgentSteer(_ context.Context, _ core.Job, delivery core.Delivery) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.submitted = append(e.submitted, delivery.Message.Sequence)
	return delivery.Message.TargetTurnID, nil
}
func (e *integrationExternals) RouteRevoke(context.Context, core.Job, core.Sandbox, core.Route) error {
	return e.effect(core.ActionRouteRevoke)
}
func (e *integrationExternals) SandboxDelete(context.Context, core.Job, core.Sandbox) error {
	return e.effect(core.ActionSandboxDelete)
}
func (e *integrationExternals) submittedSequences() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int64(nil), e.submitted...)
}

func (e *integrationExternals) submittedInputs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.inputs...)
}

func (e *integrationExternals) effectKinds() []core.ActionKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]core.ActionKind(nil), e.effects...)
}

func TestJobTaskAttachmentFencesStaleEffectsAndAgentSelection(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	input := codingJobInput("reattach-cas-"+suffix, "preserve one task binding", "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", "dorf/reattach-cas")
	job, created, err := store.AdmitCoding(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("initial delivery=%#v err=%v", deliveries, err)
	}
	follow, followCreated, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{
		JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
		FromID: "stale-task-early-follow", Input: "preserve this accepted follow",
	})
	if err != nil || !followCreated {
		t.Fatalf("early follow=%#v created=%v err=%v", follow, followCreated, err)
	}
	if err := store.PrepareAgentRun(ctx, deliveries[0].AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, deliveries[0].AgentRun.ID, "codex", "thread-authoritative", "turn-initial", "completed"); err != nil {
		t.Fatal(err)
	}
	owned, err := store.Sandbox(ctx, core.MainSandboxName(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	action, err := store.GetOrCreateSandboxAction(ctx, owned.ID, core.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	externals := &integrationExternals{}
	agents := &countingCodingAgentExecution{codingAgentExecution: codingAgentExecution{store: store, externals: externals}}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).WithAgentExecution(agents)
	staleTaskName := "stale-provider-effect-v1"
	client.MustRegister(absurd.Task(staleTaskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		if err := service.ReconcileJobAgent(taskCtx, job.ID); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("unattached task reached Agent Message selection")
		}
		if err := service.ExecuteSandboxAction(taskCtx, job.ID, owned.ID, action.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("unattached task acquired provider authority")
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "stale-refused"}, nil
	}))
	_, err = client.Spawn(ctx, staleTaskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: staleTaskName + ":" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := client.Spawn(ctx, "unrelated-task", core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{QueueName: client.QueueName(), IdempotencyKey: "unrelated:" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), client.QueueName(), unrelated.TaskID) })
	if err := store.AttachJobTask(ctx, job.ID, "", unrelated.TaskID, "unrelated-task"); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "stale-effect-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := externals.effectKinds(); len(got) != 0 {
		t.Fatalf("unattached stale task mutated provider: %v", got)
	}
	if agents.selections != 0 {
		t.Fatalf("unattached stale task invoked Agent selector %d times", agents.selections)
	}
	unchanged, err := store.AgentMessageExecution(ctx, follow.ID)
	if err != nil || unchanged.AgentRun.ThreadID != "" {
		t.Fatalf("stale task changed early follow Thread binding: %#v err=%v", unchanged.AgentRun, err)
	}
	if err := client.CancelTask(ctx, client.QueueName(), unrelated.TaskID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, coding.TaskName, core.JobTaskParams{JobID: job.ID, PreviousTaskID: ""}, absurd.SpawnOptions{IdempotencyKey: coding.TaskKey(job.ID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), client.QueueName(), spawned.TaskID) })
	if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, coding.TaskName); err == nil {
		t.Fatal("a second public Spawn result replaced the stored task binding")
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "stale-spawn-ack-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	result, err := client.FetchTaskResult(ctx, client.QueueName(), spawned.TaskID)
	if err != nil || result == nil || result.State == absurd.TaskCompleted {
		t.Fatalf("stale Spawn acknowledgment task result=%#v err=%v", result, err)
	}
}

func TestPostgresJobFenceSerializesOverlappingClaims(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	firstEntered := make(chan struct{})
	release := make(chan struct{})
	secondEntered := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		errs <- store.WithJobFence(ctx, "job-fence-integration", func() error { close(firstEntered); <-release; return nil })
	}()
	<-firstEntered
	go func() {
		errs <- store.WithJobFence(ctx, "job-fence-integration", func() error { close(secondEntered); return nil })
	}()
	select {
	case <-secondEntered:
		close(release)
		t.Fatal("second claim crossed the PostgreSQL Job execution fence")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestClosedAdmissionRejectsLateRevisionObservation(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	observedJob, threadID := prepareTransportIntegrationJob(t, store, "closed-revision-observation")
	run := completeNextIntegrationRun(t, store, observedJob.ID, threadID, "turn-closed-observation")
	observation := gitworkspace.Observation{
		ComparisonBase: observedJob.Revision, Revision: strings.Repeat("9", 40), Tree: strings.Repeat("8", 40),
		Branch: observedJob.Branch, StartedAt: now, FinishedAt: now,
	}
	observedEvidence := integrationEvidence(run.ID, "git-revision", "", "", observation.Revision, "2")
	observedEvidence.AgentRunID = run.ID
	if err := store.RequestCleanup(ctx, observedJob.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRevisionObservation(ctx, observedJob.ID, run.ID, observation, observedEvidence); !errors.Is(err, postgres.ErrRevisionObservationSuperseded) {
		t.Fatalf("closed admission Revision observation error=%v", err)
	}
}

func TestCleanupCompletesWithExplanatoryWorkflowAttention(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _ := prepareTransportIntegrationJob(t, store, "cleanup-attention")
	if err := store.SetWorkflowAttention(ctx, job.ID, "operator:test", "explanatory only"); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		if err := store.InterruptAgentRun(ctx, delivery.AgentRun.ID, "cleanup"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, job.ID, job.CurrentTaskID, "cleanup-task-"+job.ID, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	sandboxID := core.MainSandboxName(job.ID)
	for _, kind := range []core.ActionKind{core.ActionRouteRevoke, core.ActionSandboxDelete} {
		action, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CompleteCleanup(ctx, job.ID, "cleanup-task-"+job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteCleanup(ctx, job.ID, "cleanup-task-"+job.ID); err != nil {
		t.Fatalf("exact cleanup completion replay failed: %v", err)
	}
	if err := store.CompleteCleanup(ctx, job.ID, "other-cleanup-task"); err == nil {
		t.Fatal("foreign cleanup task replay was accepted")
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete || cleaned.WorkflowAttention != "" || cleaned.WorkflowAttentionSource != "" || !cleaned.WorkflowAttentionAt.IsZero() {
		t.Fatalf("cleanup terminal retained explanatory attention: job=%#v err=%v", cleaned, err)
	}
}
