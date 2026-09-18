package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type providerCheck struct {
	err error
}

func nextDelivery(ctx context.Context, store postgres.Store, sessionID string) (*core.Delivery, error) {
	work, err := store.AgentMessage(ctx, sessionID)
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

func (providerCheck) DefaultConnection() (string, error) { return "primary", nil }

func (providerCheck) DefaultModel(string) (string, error) { return "gpt-5.6-sol", nil }

type failOnceWorkflowBarrier struct {
	mu     sync.Mutex
	point  string
	failed bool
}

type actionAttentionError string

func (e actionAttentionError) Error() string       { return string(e) }
func (actionAttentionError) AttentionNeeded() bool { return true }

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

func (e *blockingCreateExternals) SandboxCreate(ctx context.Context, session core.Session, sandbox core.Sandbox) (string, error) {
	providerID, err := e.integrationExternals.SandboxCreate(ctx, session, sandbox)
	if err != nil {
		return "", err
	}
	e.once.Do(func() { close(e.entered) })
	select {
	case <-e.release:
		return providerID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type integrationExecution interface {
	core.Execution
	core.AgentReconciliation
	core.CleanupExecution
}

type integrationRuntimeResolver struct {
	execution integrationExecution
	files     core.SandboxFileReader
	profile   string
}

func (r integrationRuntimeResolver) ResolveDirect(_ context.Context, name core.SandboxProfileRef) (direct.Runtime, error) {
	if name.Name != r.profile {
		return direct.Runtime{}, fmt.Errorf("unexpected Sandbox profile %q", name)
	}
	return direct.Runtime{SandboxProfile: name, Execution: r.execution}, nil
}

func (r integrationRuntimeResolver) ResolveSandbox(_ context.Context, name core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if name.Name != r.profile {
		return core.SandboxRuntime{}, fmt.Errorf("unexpected Sandbox profile %q", name)
	}
	return core.SandboxRuntime{Execution: r.execution, Files: r.files, SandboxProfile: name}, nil
}

func (r integrationRuntimeResolver) ResolveCleanup(_ context.Context, name core.SandboxProfileRef) (core.CleanupRuntime, error) {
	if name.Name != r.profile {
		return core.CleanupRuntime{}, fmt.Errorf("unexpected Sandbox profile %q", name)
	}
	return core.CleanupRuntime{Execution: r.execution, SandboxProfile: name}, nil
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
	profile, _, err := store.CreateSandboxProfile(context.Background(), completeIncusProfile("incus", "codex", strings.Repeat("a", 64)))
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
	externals := &integrationExternals{turnStatus: "completed"}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(integrationAgentExecution{store: store, externals: externals})
	runtimeProfile := "incus"
	resolver := integrationRuntimeResolver{
		execution: execution,
		profile:   runtimeProfile,
	}
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver, CleanupRuntimes: resolver}
	application.RegisterCleanup()
	direct.Register(application, store, resolver)
	t.Cleanup(func() {
		if err := client.DropQueue(context.Background(), queueName); err != nil {
			t.Errorf("drop test queue %q: %v", queueName, err)
		}
		client.Close()
		db.Close()
	})
	return db, store, client
}

func TestWorkflowEnsureAndCleanupSerializeBothWinnerOrders(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	externals := &blockingCreateExternals{
		integrationExternals: &integrationExternals{}, entered: make(chan struct{}), release: make(chan struct{}),
	}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(integrationAgentExecution{store: store, externals: externals.integrationExternals})
	profile := "incus"
	resolver := integrationRuntimeResolver{
		execution: execution, profile: profile,
	}
	client := newFaultClient(t, store, fmt.Sprintf("dorf_workflow_cleanup_race_%d", time.Now().UnixNano()))
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver, CleanupRuntimes: resolver}
	application.RegisterCleanup()
	direct.Register(application, store, resolver)
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- client.RunWorker(workerCtx, absurd.WorkerOptions{WorkerID: "workflow-cleanup-race", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 1})
	}()
	t.Cleanup(func() { stopWorker(); <-workerDone })

	session, created, err := store.AdmitDirect(ctx, directSessionInput(
		fmt.Sprintf("ensure-wins-%d", time.Now().UnixNano()),
	), client.QueueName())
	if err != nil || !created {
		t.Fatalf("admit ensure winner created=%t err=%v", created, err)
	}
	select {
	case <-externals.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("workflow Sandbox ensure did not enter provider")
	}
	handle, err := application.OpenSession(ctx, session.ID)
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
	cleaning, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), cleaning.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Session(ctx, session.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("cleanup did not inventory winning ensure: %#v err=%v", cleaned, err)
	}
	got := externals.effectKinds()
	if len(got) < 3 || got[0] != core.ActionSandboxCreate || got[len(got)-2] != core.ActionRouteRevoke || got[len(got)-1] != core.ActionSandboxDelete {
		t.Fatalf("ensure-winner provider effects=%v", got)
	}

	loserExternals := &integrationExternals{}
	loserExecution := core.NewExecutionService(store, loserExternals, nil, absurdruntime.RequireClaim)
	loserResolver := integrationRuntimeResolver{
		execution: loserExecution, profile: profile,
	}
	loserClient := newFaultClient(t, store, fmt.Sprintf("dorf_cleanup_wins_%d", time.Now().UnixNano()))
	loserApplication := core.Application{Store: store, Tasks: loserClient, SandboxRuntimes: loserResolver, CleanupRuntimes: loserResolver}
	loserApplication.RegisterCleanup()
	direct.Register(loserApplication, store, loserResolver)
	loser, created, err := store.AdmitDirect(ctx, directSessionInput(
		fmt.Sprintf("cleanup-wins-%d", time.Now().UnixNano()),
	), loserClient.QueueName())
	if err != nil || !created {
		t.Fatalf("admit cleanup winner created=%t err=%v", created, err)
	}
	loserHandle, err := loserApplication.OpenSession(ctx, loser.ID)
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

func directSessionInput(key string) core.SessionAdmission {
	return core.SessionAdmission{AdmissionKey: key, SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high"}
}

func TestPostgresDirectBootstrapFollowAndExplicitCleanup(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	session, created, err := direct.NewAdmissionService(store, client.QueueName(), providerCheck{}).Admit(ctx, direct.AdmissionRequest{
		AdmissionKey: fmt.Sprintf("direct-execution-%d", time.Now().UnixNano()),
		AgentsMD:     "prove the direct client execution boundary", SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
	})
	if err != nil || !created || session.CurrentTaskID == "" {
		t.Fatalf("direct admission Session=%#v created=%t err=%v", session, created, err)
	}
	initialTaskID := session.CurrentTaskID
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "direct-execution", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	session, err = store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := store.Actions(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !session.AdmissionOpen || session.CleanupState != core.CleanupPending || session.WorkflowAttention != "" || len(deliveries) != 0 || len(actions) != 2 {
		t.Fatalf("Session setup should be idle with no Messages: session=%#v actions=%#v deliveries=%#v", session, actions, deliveries)
	}
	idleTask, err := client.FetchTaskResult(ctx, client.QueueName(), session.CurrentTaskID)
	if err != nil || idleTask == nil || idleTask.State != absurd.TaskSleeping {
		t.Fatalf("open-idle direct Absurd task=%#v err=%v", idleTask, err)
	}

	application := core.Application{Store: store, Tasks: client, AgentMessages: directMessageAdmissions{store: store}}
	handle, err := application.OpenSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := handle.DefaultSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := sandbox.Agent().Message(ctx, "direct-message", core.MessageInput{Text: "prove the direct client execution boundary"})
	if err != nil || !first.Created || first.Sequence != 1 {
		t.Fatalf("first Message=%#v err=%v", first, err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "direct-message", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	deliveries, err = store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].AgentRun.State != core.AgentRunCompleted || deliveries[0].AgentRun.StartedAt.IsZero() || actions[1].SettledAt.After(deliveries[0].AgentRun.StartedAt) {
		t.Fatalf("ordinary Message must run after preparation: deliveries=%#v err=%v", deliveries, err)
	}
	accepted, err := sandbox.Agent().Message(ctx, "direct-follow", core.MessageInput{Text: "continue in the retained Thread"})
	if err != nil || !accepted.Created {
		t.Fatalf("direct Follow=%#v err=%v", accepted, err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "direct-follow", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	session, err = store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	deliveries, err = store.Deliveries(ctx, session.ID)
	if err != nil || session.CurrentTaskID != initialTaskID || len(deliveries) != 2 ||
		deliveries[1].AgentRun.State != core.AgentRunCompleted || deliveries[1].AgentRun.ThreadID == "" ||
		deliveries[1].AgentRun.ThreadID != deliveries[0].AgentRun.ThreadID {
		t.Fatalf("direct Follow did not reuse the exact task and Thread: session=%#v deliveries=%#v err=%v", session, deliveries, err)
	}

	if err := handle.RequestCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	requested, err := store.Session(ctx, session.ID)
	if err != nil || requested.AdmissionOpen || requested.CleanupState != core.CleanupScheduled || requested.CurrentTaskID == initialTaskID {
		t.Fatalf("explicit direct cleanup request=%#v err=%v", requested, err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "direct-cleanup", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Session(ctx, session.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("explicit direct cleanup=%#v err=%v", cleaned, err)
	}
	resources, err := store.SandboxResources(ctx, session.ID)
	if err != nil || len(resources) != 1 || resources[0].ProviderID == "" || resources[0].ObservedAt.IsZero() || resources[0].DeletedAt.IsZero() {
		t.Fatalf("cleanup did not retain the observed resource and deletion receipt: count=%d err=%v", len(resources), err)
	}
}

type directMessageAdmissions struct{ store postgres.Store }

func (a directMessageAdmissions) AdmitAgentMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	return a.store.AdmitDirectMessage(ctx, input)
}

func TestPostgresDirectAdmissionReplayRecoversTaskAttachment(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	input := core.SessionAdmission{
		AdmissionKey:   fmt.Sprintf("direct-%d", time.Now().UnixNano()),
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
	}
	session, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || !created || session.Workflow != "" || session.WorkflowRevision != "" {
		t.Fatalf("direct admission session=%#v created=%t err=%v", session, created, err)
	}
	request := direct.AdmissionRequest{
		AdmissionKey: input.AdmissionKey, AgentsMD: input.AgentsMD, SandboxProfile: input.SandboxProfile,
		ProviderConnection: input.ProviderConnection, Model: input.Model, ReasoningEffort: input.ReasoningEffort,
	}
	recovered, created, err := direct.NewAdmissionService(
		store, client.QueueName(),
		providerCheck{err: errors.New("provider unavailable during admission recovery")},
	).Admit(ctx, request)
	if err != nil || created || recovered.ID != session.ID || recovered.CurrentTaskID == "" {
		t.Fatalf("client scheduling recovery session=%#v created=%t err=%v", recovered, created, err)
	}
	replayed, created, err := direct.NewAdmissionService(
		store, client.QueueName(),
		providerCheck{err: errors.New("provider unavailable during replay")},
	).Admit(ctx, request)
	if err != nil || created || replayed.ID != session.ID || replayed.CurrentTaskID != recovered.CurrentTaskID {
		t.Fatalf("client scheduled replay session=%#v created=%t err=%v", replayed, created, err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != 1 || deliveries[0].AgentRun.Role != direct.DirectAgentRole ||
		deliveries[0].AgentRun.Capability != "" || deliveries[0].AgentRun.InputRevision != "" ||
		deliveries[0].AgentRun.SandboxID != core.MainSandboxName(session.ID) {
		t.Fatalf("initial direct delivery=%#v err=%v", deliveries, err)
	}
}

func requestCleanupIntegration(t *testing.T, application core.Application, sessionID string) core.Session {
	t.Helper()
	handle, err := application.OpenSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.RequestCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	session, err := application.Store.Session(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestPostgresMessageIdempotencyConcurrentFIFOAndLowestUnsettled(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("message-integration-%d", time.Now().UnixNano())
	session, created, err := store.AdmitDirect(ctx, directSessionInput(key), client.QueueName())
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	taskIDs := []string{session.CurrentTaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), client.QueueName(), id)
		}
	})

	first, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "client-retry", Input: "same text"})
	if err != nil || !first.Created || first.Message.Sequence != 1 || first.Message.FromKind != "human" || first.Message.FromID != "client-retry" || first.Message.ID != core.MessageID(session.ID, "human", "client-retry") {
		t.Fatalf("first message=%#v err=%v", first, err)
	}
	repeated, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "client-retry", Input: "same text"})
	if err != nil || repeated.Created || !reflect.DeepEqual(repeated.Message, first.Message) {
		t.Fatalf("idempotent message=%#v err=%v", repeated, err)
	}
	if admitted, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: "foreign-sandbox", FromKind: "human", FromID: "client-retry", Input: "same text"}); !errors.Is(err, core.ErrMessageReplayConflict) || admitted.Created {
		t.Fatalf("same send key replayed through another Sandbox: admitted=%#v err=%v", admitted, err)
	}
	if _, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "client-retry", Input: "changed"}); !errors.Is(err, core.ErrMessageReplayConflict) {
		t.Fatalf("changed input replay error=%v", err)
	}
	if _, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "client-retry", Input: "same text "}); !errors.Is(err, core.ErrMessageReplayConflict) {
		t.Fatalf("byte-distinct input replay error=%v", err)
	}
	distinct, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "client-distinct", Input: "same text"})
	if err != nil || !distinct.Created || distinct.Message.ID == first.Message.ID || distinct.Message.Sequence != 2 {
		t.Fatalf("distinct identical message=%#v err=%v", distinct, err)
	}
	crossKind, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "workflow", FromID: distinct.Message.FromID, Input: "same source identity from the workflow"})
	if err != nil || !crossKind.Created || crossKind.Message.Sequence != 3 || crossKind.Message.ID == distinct.Message.ID || crossKind.Message.ID != core.MessageID(session.ID, "workflow", distinct.Message.FromID) || crossKind.Message.FromKind != "workflow" || crossKind.Message.FromID != distinct.Message.FromID {
		t.Fatalf("cross-kind source identity=%#v err=%v", crossKind, err)
	}

	const concurrent = 12
	sequences := make(chan int64, concurrent)
	errResults := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := range concurrent {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			admitted, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: fmt.Sprintf("concurrent-%02d", i), Input: "same concurrent text"})
			if err == nil {
				sequences <- admitted.Message.Sequence
			}
			errResults <- err
		}(i)
	}
	wg.Wait()
	close(sequences)
	close(errResults)
	for err := range errResults {
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
		if sequence != i+4 {
			t.Fatalf("concurrent FIFO positions=%v", got)
		}
	}

	threadID := "thread-" + session.ID
	delivery, err := nextDelivery(ctx, store, session.ID)
	if err != nil || delivery.Message.Sequence != 1 {
		t.Fatalf("lowest delivery=%#v err=%v", delivery, err)
	}
	if delivery.AgentRun.SandboxID != core.MainSandboxName(session.ID) {
		t.Fatalf("delivery Sandbox=%q want=%q", delivery.AgentRun.SandboxID, core.MainSandboxName(session.ID))
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", threadID, "turn-"+session.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	next, err := nextDelivery(ctx, store, session.ID)
	if err != nil || next.Message.Sequence != 2 || next.AgentRun.ID == delivery.AgentRun.ID {
		t.Fatalf("next delivery=%#v err=%v", next, err)
	}
	if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", "turn-"+session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-2-"+session.ID, "running"); err != nil {
		t.Fatal(err)
	}
	blockers, err := store.UnsettledAgentMessages(ctx, session.ID)
	if err != nil || len(blockers) != 1 || blockers[0].MessageID != next.Message.ID || blockers[0].SandboxID != core.MainSandboxName(session.ID) {
		t.Fatalf("active harness mutations=%#v err=%v", blockers, err)
	}
	stillOpen, err := store.Session(ctx, session.ID)
	if err != nil || !stillOpen.AdmissionOpen {
		t.Fatalf("harness mutation inspection changed admission: %#v err=%v", stillOpen, err)
	}
	if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-2-"+session.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	fenceEntered := make(chan struct{})
	releaseFence := make(chan struct{})
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- store.WithSessionFence(ctx, session.ID, func() error {
			close(fenceEntered)
			<-releaseFence
			return nil
		})
	}()
	<-fenceEntered
	type cleanupResult struct {
		session core.Session
		err     error
	}
	cleanupDone := make(chan cleanupResult, 1)
	go func() {
		application := core.Application{Store: store, Tasks: client}
		handle, err := application.OpenSession(ctx, session.ID)
		if err == nil {
			err = handle.RequestCleanup(ctx)
		}
		var cleaning core.Session
		if err == nil {
			cleaning, err = store.Session(ctx, session.ID)
		}
		cleanupDone <- cleanupResult{session: cleaning, err: err}
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
	cleaning := cleanup.session
	taskIDs = append(taskIDs, cleaning.CurrentTaskID)
	if cleaning.AdmissionOpen {
		t.Fatal("cleanup did not durably close admission")
	}
	if retry, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "client-retry", Input: "same text"}); err != nil || retry.Created || !reflect.DeepEqual(retry.Message, first.Message) {
		t.Fatalf("closed admission did not preserve idempotent retry: %#v %v", retry, err)
	}
	if _, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "after-cleanup", Input: "late"}); !errors.Is(err, core.ErrMessageAdmissionClosed) {
		t.Fatalf("cleanup admission error=%v", err)
	}
}

func TestSandboxProfileVerificationHasOneOwnerAndReleasesAfterCrash(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("verify-owner-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, completeIncusProfile(name, "codex", strings.Repeat("d", 64))); err != nil {
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
	input := directSessionInput("verification-fence-" + name)
	input.SandboxProfile = name
	session, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || !created || session.SandboxProfile != name || resumed.OwnershipNonce != first.OwnershipNonce {
		t.Fatalf("admission after resumed verification Session=%#v created=%v resumed=%#v err=%v", session, created, resumed, err)
	}
}

func TestSandboxProfileVerificationTransitionSerializesNewAdmission(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("verify-serial-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, completeIncusProfile(name, "codex", strings.Repeat("f", 64))); err != nil {
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
	input := directSessionInput("verification-serialization-" + name)
	input.SandboxProfile = name
	type admissionResult struct {
		created bool
		err     error
	}
	started := make(chan struct{})
	admitted := make(chan admissionResult, 1)
	go func() {
		close(started)
		_, created, err := admitDirectFixture(t, store, ctx, input)
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
	if _, err := transition.ExecContext(ctx, `
insert into dorf.sandbox_profile_verifications(profile_name,contract_version,definition_hash,sandbox_id,ownership_nonce)
select name,$2,candidate_revision,$3,$4 from dorf.sandbox_profiles where name=$1
`, name, core.BaseProfileContract, "transition-"+name, nonce); err != nil {
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

func TestSandboxProfilesPromoteVerifiedRevisionsWhileSessionsRemainInUse(t *testing.T) {
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

	input := directSessionInput("profile-immutability-" + name)
	input.SandboxProfile = name
	session, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	reverifying, activeVerification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || reverifying.BaseVerified() || activeVerification.OwnershipNonce == refreshedVerification.OwnershipNonce {
		t.Fatalf("active Session fresh verification profile=%#v receipt=%#v err=%v", reverifying, activeVerification, err)
	}
	replayed, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || created || replayed.ID != session.ID {
		t.Fatalf("existing admission replay during verification Session=%#v created=%v err=%v", replayed, created, err)
	}
	fenced := input
	fenced.AdmissionKey += "-during-reverify"
	if _, _, err := admitDirectFixture(t, store, ctx, fenced); err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("new Session admitted through unsettled verification: %v", err)
	}
	verificationFailure := errors.New("transient verification failure")
	if err := store.RecordSandboxProfileVerificationError(ctx, activeVerification, verificationFailure); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, activeVerification); err != nil {
		t.Fatal(err)
	}
	if _, _, err := admitDirectFixture(t, store, ctx, fenced); err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("new Session admitted through failed verification: %v", err)
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
	admittedAfterRetry, created, err := admitDirectFixture(t, store, ctx, fenced)
	if err != nil || !created || admittedAfterRetry.SandboxProfile != name {
		t.Fatalf("admission after verification retry Session=%#v created=%v err=%v", admittedAfterRetry, created, err)
	}
	sameGateway := profile.E2BGatewayURL
	unchanged, updated, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{E2BGatewayURL: &sameGateway})
	if err != nil || updated || !unchanged.Default || !unchanged.BaseVerified() {
		t.Fatalf("no-op patch changed verified default profile: updated=%v profile=%#v err=%v", updated, unchanged, err)
	}
	changedGateway := "https://replacement.example/v1"
	changed, updated, err := store.UpdateSandboxProfile(ctx, name, postgres.SandboxProfilePatch{E2BGatewayURL: &changedGateway})
	if err != nil || !updated || changed.BaseVerified() || !changed.Default {
		t.Fatalf("stage: %+v %v", changed, err)
	}
	active, err := store.ActiveSandboxProfile(ctx, name)
	if err != nil || active.DefinitionHash != session.SandboxProfileRevision || !active.BaseVerified() {
		t.Fatalf("staging replaced active revision: %+v %v", active, err)
	}
	pinned, err := store.SandboxProfileRevision(ctx, session.ProfileRef())
	if err != nil || pinned.E2BGatewayURL != profile.E2BGatewayURL {
		t.Fatalf("existing Session changed: %+v %v", pinned, err)
	}
	during := input
	during.AdmissionKey += "-while-staged"
	duringSession, _, err := admitDirectFixture(t, store, ctx, during)
	if err != nil || duringSession.SandboxProfileRevision != session.SandboxProfileRevision {
		t.Fatalf("staged admission: %+v %v", duringSession, err)
	}
	_, candidateProof, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationError(ctx, candidateProof, errors.New("candidate probe failed")); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, candidateProof); err != nil {
		t.Fatal(err)
	}
	active, err = store.ActiveSandboxProfile(ctx, name)
	if err != nil || active.DefinitionHash != session.SandboxProfileRevision || !active.BaseVerified() {
		t.Fatalf("failed candidate replaced active: %+v %v", active, err)
	}
	_, candidateProof, err = store.BeginSandboxProfileVerification(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxProfileProbe(ctx, candidateProof, "pi upgraded"); err != nil {
		t.Fatal(err)
	}
	active, err = store.ActiveSandboxProfile(ctx, name)
	if err != nil || active.DefinitionHash != session.SandboxProfileRevision {
		t.Fatalf("promoted before cleanup: %+v %v", active, err)
	}
	if err := store.RecordSandboxProfileVerificationError(ctx, candidateProof, errors.New("cleanup temporarily unavailable")); err != nil {
		t.Fatal(err)
	}
	_, resumedProof, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil || resumedProof.OwnershipNonce != candidateProof.OwnershipNonce || resumedProof.ProbeCompletedAt.IsZero() {
		t.Fatalf("cleanup retry=%+v err=%v", resumedProof, err)
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, resumedProof); err != nil {
		t.Fatal(err)
	}
	active, err = store.DefaultSandboxProfile(ctx)
	if err != nil || active.DefinitionHash != changed.DefinitionHash || !active.Default || !active.BaseVerified() {
		t.Fatalf("promotion/default: %+v %v", active, err)
	}
	after := input
	after.AdmissionKey += "-after-promotion"
	newSession, _, err := admitDirectFixture(t, store, ctx, after)
	if err != nil || newSession.SandboxProfileRevision != changed.DefinitionHash {
		t.Fatalf("new admission: %+v %v", newSession, err)
	}
	replay, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || created || replay.ProfileRef() != session.ProfileRef() {
		t.Fatalf("replay rebound old Session: %+v %v", replay, err)
	}
	if err := store.RecordSandboxProfileUnavailable(ctx, session.ID, name, session.ID, errors.New("old artifact unavailable")); err != nil {
		t.Fatal(err)
	}
	active, err = store.ActiveSandboxProfile(ctx, name)
	if err != nil || !active.BaseVerified() {
		t.Fatalf("old failure invalidated new revision: %+v %v", active, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.sessions set sandbox_profile_revision=$2 where id=$1`, session.ID, changed.DefinitionHash); err == nil {
		t.Fatal("accepted rewriting a Session binding")
	}
	if _, err := db.ExecContext(ctx, `update dorf.sandbox_profile_revisions set artifact='changed' where name=$1`, name); err == nil {
		t.Fatal("accepted mutating an immutable revision")
	}
	// No Session cleanup was required to stage, verify, promote, admit, or replay.
	persisted, err := store.Session(ctx, session.ID)
	if err != nil || persisted.CleanupState != core.CleanupPending {
		t.Fatalf("old Session was cleaned: %+v %v", persisted, err)
	}

}

func TestSandboxProfileUpdateInvalidatesActiveVerification(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	name := fmt.Sprintf("verification-update-%d", time.Now().UnixNano())
	original := completeIncusProfile(name, "codex", strings.Repeat("b", 64))
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

func TestUnavailableSandboxProfileFencesNewSessionsAndPreservesExactAttention(t *testing.T) {
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
	input := directSessionInput("profile-unavailable-" + name)
	input.SandboxProfile = name
	session, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	source := core.ScopedActionID(session.ID, core.ActionSandboxCreate, core.MainSandboxName(session.ID))
	failure := provider.ArtifactUnavailableErrorf("E2B template %q is unavailable", "dorf/missing:exact-build")
	if err := store.RecordSandboxProfileUnavailable(ctx, session.ID, name, source, failure); err != nil {
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
	stopped, err := store.Session(ctx, session.ID)
	if err != nil || stopped.WorkflowAttentionSource != source || stopped.WorkflowAttention != failure.Error() {
		t.Fatalf("stopped Session=%#v err=%v", stopped, err)
	}
	newInput := input
	newInput.AdmissionKey += "-new"
	if _, _, err := admitDirectFixture(t, store, ctx, newInput); err == nil || !strings.Contains(err.Error(), core.BaseProfileContract) {
		t.Fatalf("new Session admitted through unavailable profile: %v", err)
	}
	cleaning := requestCleanupIntegration(t, core.Application{Store: store, Tasks: client}, session.ID)
	if cleaning.CleanupState != core.CleanupScheduled {
		t.Fatalf("cleanup from unavailable profile state=%q", cleaning.CleanupState)
	}
}

func TestSandboxProfileSchemaRejectsNullRequiredFacts(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	for _, statement := range []string{
		`insert into dorf.sandbox_profile_revisions(name,provider,harness,artifact,incus_disk_size) values('invalid-incus-null','incus','codex',repeat('d',64),'40GiB')`,
		`insert into dorf.sandbox_profile_revisions(name,provider,harness,artifact,e2b_sandbox_timeout_seconds,e2b_allow_internet) values('invalid-e2b-null','e2b','codex','dorf:build',3300,false)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted incomplete profile: %s", statement)
		}
	}
	name := fmt.Sprintf("invalid-verification-%d", time.Now().UnixNano())
	if _, _, err := store.CreateSandboxProfile(ctx, completeIncusProfile(name, "codex", strings.Repeat("e", 64))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
			insert into dorf.sandbox_profile_verifications(
				profile_name,contract_version,definition_hash,sandbox_id,ownership_nonce,probe_completed_at
			)
			select name,'base-1',candidate_revision,$2,$3,clock_timestamp()
			from dorf.sandbox_profiles where name=$1`, name, "sandbox-"+name, strings.Repeat("f", 64)); err == nil {
		t.Fatal("schema accepted a completed profile probe without a Harness version")
	}
}

func TestExplicitSteerTargetsAndAcknowledgesExactActiveTurn(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, threadID := prepareTransportIntegrationSession(t, store, "explicit-steer")
	active, err := nextDelivery(ctx, store, session.ID)
	if err != nil || active == nil {
		t.Fatalf("initial delivery=%#v err=%v", active, err)
	}
	if err := store.PrepareAgentRun(ctx, active.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	activeTurnID := "turn-active-" + session.ID
	if err := store.BindAgentRun(ctx, active.AgentRun.ID, "codex", threadID, activeTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	if candidate, err := nextDelivery(ctx, store, session.ID); err != nil || candidate == nil || candidate.Message.ID != active.Message.ID {
		t.Fatalf("active Turn reconciliation candidate=%#v err=%v", candidate, err)
	}

	steerInput := core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "operator-steer", Input: "correct the active work", Intent: core.MessageSteer}
	steer, err := store.AdmitDirectMessage(ctx, steerInput)
	if err != nil || !steer.Created || steer.Message.Intent != core.MessageSteer || steer.Message.TargetTurnID != activeTurnID {
		t.Fatalf("steer=%#v err=%v", steer, err)
	}
	repeated, err := store.AdmitDirectMessage(ctx, steerInput)
	if err != nil || repeated.Created || !reflect.DeepEqual(repeated.Message, steer.Message) {
		t.Fatalf("idempotent steer=%#v err=%v", repeated, err)
	}
	changed := steerInput
	changed.Intent = core.MessageFollow
	if _, err := store.AdmitDirectMessage(ctx, changed); !errors.Is(err, core.ErrMessageReplayConflict) {
		t.Fatalf("changed delivery replay error=%v", err)
	}
	delivery, err := nextDelivery(ctx, store, session.ID)
	if err != nil || delivery == nil || delivery.Message.ID != steer.Message.ID || delivery.AgentRun.ThreadID != threadID {
		t.Fatalf("steer delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", activeTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, delivery.AgentRun.ID, activeTurnID, "inProgress"); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != 2 || deliveries[1].AgentRun.TurnID != activeTurnID || deliveries[1].Message.Intent != core.MessageSteer {
		t.Fatalf("steer deliveries=%#v err=%v", deliveries, err)
	}
	if err := store.BindAgentRun(ctx, active.AgentRun.ID, "codex", threadID, activeTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, delivery.AgentRun.ID, activeTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	repeated, err = store.AdmitDirectMessage(ctx, steerInput)
	if err != nil || repeated.Created || !reflect.DeepEqual(repeated.Message, steer.Message) {
		t.Fatalf("terminal-target replay retargeted or reauthorized: Message=%#v err=%v", repeated, err)
	}
	next, err := nextDelivery(ctx, store, session.ID)
	if err != nil || next != nil {
		t.Fatalf("delivery after steer=%#v err=%v, want active Turn observation", next, err)
	}
	other, _ := prepareTransportIntegrationSession(t, store, "steer-without-active-turn")
	if _, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: other.ID, SandboxID: core.MainSandboxName(other.ID), FromKind: "human", FromID: "invalid-steer", Input: "cannot target", Intent: core.MessageSteer}); !errors.Is(err, core.ErrMessageSteerUnavailable) {
		t.Fatalf("steer without active turn error=%v", err)
	}
}

func TestDeliveriesFailsLoudlyWhenMessageHasNoAgentRun(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	session, _ := prepareTransportIntegrationSession(t, store, "orphan-message-read")
	orphanID := "message-orphan-" + session.ID
	if _, err := db.ExecContext(ctx, `
		insert into dorf.session_messages(id,session_id,from_kind,from_id,sequence,input)
		values($1,$2,'human','corruption-test',2,'retained orphan input')`, orphanID, session.ID); err != nil {
		t.Fatal(err)
	}
	if deliveries, err := store.Deliveries(ctx, session.ID); err == nil || !strings.Contains(err.Error(), orphanID) {
		t.Fatalf("Deliveries=%#v error=%v, want named orphan Message failure", deliveries, err)
	}
}

func TestSharedSteersPersistEveryTerminalTargetOutcome(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			session, threadID := prepareTransportIntegrationSession(t, store, "shared-steer-outcome-"+status)
			target, err := nextDelivery(ctx, store, session.ID)
			if err != nil || target == nil {
				t.Fatalf("target delivery=%#v err=%v", target, err)
			}
			if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
				t.Fatal(err)
			}
			targetTurnID := "turn-shared-" + session.ID
			if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
				t.Fatal(err)
			}
			first, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "first-shared-steer", Input: "first accepted shared input", Intent: core.MessageSteer})
			if err != nil || !first.Created {
				t.Fatalf("first steer=%#v err=%v", first, err)
			}
			second, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "second-shared-steer", Input: "second accepted shared input", Intent: core.MessageSteer})
			if err != nil || !second.Created {
				t.Fatalf("second steer=%#v err=%v", second, err)
			}
			firstDelivery, err := nextDelivery(ctx, store, session.ID)
			if err != nil || firstDelivery == nil || firstDelivery.Message.ID != first.Message.ID {
				t.Fatalf("first steer delivery=%#v err=%v", firstDelivery, err)
			}
			if err := store.PrepareAgentRun(ctx, firstDelivery.AgentRun.ID, "codex", targetTurnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindSteer(ctx, firstDelivery.AgentRun.ID, targetTurnID, "inProgress"); err != nil {
				t.Fatal(err)
			}
			secondDelivery, err := nextDelivery(ctx, store, session.ID)
			if err != nil || secondDelivery == nil || secondDelivery.Message.ID != second.Message.ID {
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
			deliveries, err := store.Deliveries(ctx, session.ID)
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

func TestSteerTargetTerminalBeforeAcceptanceFailsWithoutNewTurn(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	session, threadID := prepareTransportIntegrationSession(t, store, "steer-terminal-failure")
	target, err := nextDelivery(ctx, store, session.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + session.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "terminal-race-steer", Input: "preserve exact durable input", Intent: core.MessageSteer})
	if err != nil || !steer.Created || steer.Message.TargetTurnID != targetTurnID {
		t.Fatalf("steer=%#v err=%v", steer, err)
	}
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	later, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "later-follow", Input: "later FIFO delivery"})
	if err != nil || !later.Created {
		t.Fatalf("later=%#v err=%v", later, err)
	}
	externals := &integrationExternals{turns: []core.HarnessTurn{{ID: targetTurnID, Status: "completed"}}}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(resultBoundaryAgentExecution{externals: externals})
	taskName := "dorf-terminal-steer-failure-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if _, err := execution.ReconcileSessionAgent(taskCtx, session.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "terminal-steer-failed"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSessionTask(ctx, session.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "terminal-steer-failure", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	if submitted := externals.submittedSequences(); len(submitted) != 0 {
		t.Fatalf("terminal-target Steer submitted a new Turn: %v", submitted)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != 3 {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	failed := deliveries[1]
	if failed.Message.ID != steer.Message.ID || failed.Message.TargetTurnID != targetTurnID || failed.AgentRun.State != core.AgentRunFailed || failed.AgentRun.TurnID != "" || !strings.Contains(failed.AgentRun.Attention, "terminal before") {
		t.Fatalf("terminal-target Steer did not fail honestly: %#v", failed)
	}
	next, err := nextDelivery(ctx, store, session.ID)
	if err != nil || next == nil || next.Message.ID != later.Message.ID || next.AgentRun.ThreadID != threadID || next.Message.Intent != core.MessageFollow {
		t.Fatalf("later Follow=%#v err=%v", next, err)
	}
}

func TestAutoSteerTargetTerminalBeforeAcceptanceRequeuesSameMessageAsFollowFIFO(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	session, threadID := prepareTransportIntegrationSession(t, store, "auto-steer-terminal-follow")
	target, err := nextDelivery(ctx, store, session.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + session.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	queued, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
		SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
		FromID: "queued-before-auto", Input: "deliver me first", Intent: core.MessageFollow,
	})
	if err != nil || !queued.Created {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	automaticInput := core.MessageAdmission{
		SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
		FromID: "terminal-race-auto", Input: "do not lose this input", Intent: core.MessageAuto,
	}
	automatic, err := store.AdmitDirectMessage(ctx, automaticInput)
	if err != nil || !automatic.Created || automatic.Message.Intent != core.MessageSteer || automatic.Message.TargetTurnID != targetTurnID {
		t.Fatalf("automatic=%#v err=%v", automatic, err)
	}
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	externals := &integrationExternals{
		turnStatus: "completed",
		turns:      []core.HarnessTurn{{ID: targetTurnID, Status: "completed"}},
	}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(resultBoundaryAgentExecution{externals: externals})
	taskName := "dorf-terminal-auto-follow-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if _, err := execution.ReconcileSessionAgent(taskCtx, session.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		if submitted := externals.submittedSequences(); len(submitted) != 0 {
			return core.TaskResultV1{}, fmt.Errorf("terminal-target automatic Message submitted before FIFO re-selection: %v", submitted)
		}
		next, err := nextDelivery(taskCtx, store, session.ID)
		if err != nil || next == nil || next.Message.ID != queued.Message.ID {
			return core.TaskResultV1{}, fmt.Errorf("next FIFO delivery=%#v err=%v", next, err)
		}
		if _, err := execution.ReconcileSessionAgent(taskCtx, session.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		next, err = nextDelivery(taskCtx, store, session.ID)
		if err != nil || next == nil || next.Message.ID != automatic.Message.ID || next.Message.Intent != core.MessageFollow || next.Message.TargetTurnID != "" {
			return core.TaskResultV1{}, fmt.Errorf("automatic follow delivery=%#v err=%v", next, err)
		}
		if _, err := execution.ReconcileSessionAgent(taskCtx, session.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "terminal-auto-follow-completed"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSessionTask(ctx, session.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "terminal-auto-follow", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	replayed, err := store.AdmitDirectMessage(ctx, automaticInput)
	if err != nil || replayed.Created || replayed.Message.ID != automatic.Message.ID || replayed.Message.Intent != core.MessageFollow || replayed.Message.TargetTurnID != "" {
		t.Fatalf("automatic replay=%#v err=%v", replayed, err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != 3 {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	if deliveries[1].Message.ID != queued.Message.ID || deliveries[2].Message.ID != automatic.Message.ID || deliveries[2].AgentRun.State != core.AgentRunCompleted || deliveries[2].AgentRun.TurnID == targetTurnID {
		t.Fatalf("same-Message automatic follow did not preserve FIFO and distinct Turn: %#v", deliveries)
	}
	if submitted := externals.submittedSequences(); !reflect.DeepEqual(submitted, []int64{queued.Message.Sequence, automatic.Message.Sequence}) {
		t.Fatalf("submitted sequences=%v", submitted)
	}
}

func TestAutoSteerErrorRequeuesAfterHistoryProvesTargetTerminalWithoutAcceptance(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	session, threadID := prepareTransportIntegrationSession(t, store, "auto-steer-error-terminal-follow")
	target, err := nextDelivery(ctx, store, session.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + session.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	automatic, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
		SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
		FromID: "steer-error-terminal-auto", Input: "preserve after uncertain acknowledgement", Intent: core.MessageAuto,
	})
	if err != nil || !automatic.Created || automatic.Message.Intent != core.MessageSteer {
		t.Fatalf("automatic=%#v err=%v", automatic, err)
	}
	externals := &integrationExternals{
		turnStatus:      "running",
		turns:           []core.HarnessTurn{{ID: targetTurnID, Status: "running"}},
		steerErr:        errors.New("steer acknowledgement lost"),
		terminalOnSteer: true,
	}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(resultBoundaryAgentExecution{externals: externals})
	taskName := "dorf-steer-error-terminal-auto-follow-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if _, err := execution.ReconcileSessionAgent(taskCtx, session.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		if _, err := execution.ReconcileSessionAgent(taskCtx, session.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		next, err := nextDelivery(taskCtx, store, session.ID)
		if err != nil || next == nil || next.Message.ID != automatic.Message.ID || next.Message.Intent != core.MessageFollow || next.Message.TargetTurnID != "" {
			return core.TaskResultV1{}, fmt.Errorf("automatic follow after steer error=%#v err=%v", next, err)
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "terminal-auto-follow-requeued"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSessionTask(ctx, session.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "steer-error-terminal-auto-follow", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	if submitted := externals.submittedSequences(); !reflect.DeepEqual(submitted, []int64{automatic.Message.Sequence}) {
		t.Fatalf("submitted sequences=%v", submitted)
	}
}

func TestTerminalHarnessTurnAllowsSameThreadFollowFIFO(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			session, threadID := prepareTransportIntegrationSession(t, store, "terminal-follow-"+status)
			first, err := nextDelivery(ctx, store, session.ID)
			if err != nil || first == nil {
				t.Fatalf("first=%#v err=%v", first, err)
			}
			if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", ""); err != nil {
				t.Fatal(err)
			}
			turnID := "turn-first-" + session.ID
			if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, turnID, "running"); err != nil {
				t.Fatal(err)
			}
			follow, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "queued-follow", Input: "continue after the accepted outcome"})
			if err != nil || !follow.Created || follow.Message.Intent != core.MessageFollow {
				t.Fatalf("follow=%#v err=%v", follow, err)
			}
			stillActive, err := nextDelivery(ctx, store, session.ID)
			if err != nil || stillActive == nil || stillActive.Message.ID != first.Message.ID {
				t.Fatalf("delivery crossed active Turn: delivery=%#v err=%v", stillActive, err)
			}
			if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, turnID, status); err != nil {
				t.Fatal(err)
			}
			next, err := nextDelivery(ctx, store, session.ID)
			if err != nil || next == nil || next.Message.ID != follow.Message.ID || next.AgentRun.ThreadID != threadID {
				t.Fatalf("follow after %s=%#v err=%v", status, next, err)
			}
			if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", turnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-follow-"+session.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			deliveries, err := store.Deliveries(ctx, session.ID)
			if err != nil || len(deliveries) != 2 || deliveries[0].AgentRun.TurnOutcome != status || deliveries[0].AgentRun.TurnID == "" || deliveries[1].AgentRun.State != core.AgentRunCompleted {
				t.Fatalf("preserved %s then follow=%#v err=%v", status, deliveries, err)
			}
		})
	}
}

func TestEarlyDirectFollowsRecoverInitialAcceptanceAndContinueSessionThread(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	session, created, err := admitDirectFixture(t, store, ctx, directSessionInput(
		fmt.Sprintf("early-follow-%d", time.Now().UnixNano()),
	))
	if err != nil || !created {
		t.Fatalf("admit Session=%#v created=%t err=%v", session, created, err)
	}
	wantInputs := []string{"initial input", "first early follow", "second early follow"}
	for i, input := range wantInputs[1:] {
		if admitted, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
			SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
			FromID: fmt.Sprintf("early-follow-%d", i+1), Input: input,
		}); err != nil || !admitted.Created {
			t.Fatalf("admit early follow %d admitted=%#v err=%v", i+1, admitted, err)
		}
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
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
	for _, kind := range []core.ActionKind{core.ActionSandboxCreate, core.ActionRouteCreate} {
		action, err := store.GetOrCreateSandboxAction(ctx, core.MainSandboxName(session.ID), kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}

	externals := &integrationExternals{turnStatus: "completed"}
	// Native acceptance survives a worker loss before the local binding is saved.
	initial, err := store.AgentMessageExecution(ctx, messageIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareAgentRun(ctx, initial.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := (integrationAgentOperation{externals: externals, execution: initial}).Submit(ctx, initial.AgentRun, wantInputs[0]); err != nil {
		t.Fatal(err)
	}
	unbound, err := store.Session(ctx, session.ID)
	if err != nil || unbound.ThreadID != "" {
		t.Fatalf("unacknowledged initial acceptance: session=%+v err=%v", unbound, err)
	}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(integrationAgentExecution{store: store, externals: externals})
	taskName := "dorf-early-follow-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		for _, messageID := range messageIDs {
			if _, err := execution.ReconcileSessionAgent(taskCtx, session.ID); err != nil {
				return core.TaskResultV1{}, err
			}
			result, err := execution.ObserveSettledAgentMessage(taskCtx, session.ID, messageID)
			if err != nil {
				return core.TaskResultV1{}, err
			}
			if !result.Terminal() || result.MessageID != messageID {
				return core.TaskResultV1{}, fmt.Errorf("Message %s did not reconcile terminally: %#v", messageID, result)
			}
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "early-follows-reconciled"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSessionTask(ctx, session.ID, "", spawned.TaskID, taskName); err != nil {
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
	deliveries, err = store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != len(wantInputs) {
		t.Fatalf("settled deliveries=%#v err=%v", deliveries, err)
	}
	bound, err := store.Session(ctx, session.ID)
	if err != nil || bound.Harness != "codex" || bound.ThreadID == "" || externals.initialStarts != 1 {
		t.Fatalf("recovered Session binding=%+v initial starts=%d err=%v", bound, externals.initialStarts, err)
	}
	threadID := bound.ThreadID
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

func TestConcurrentNativeBindingsKeepOneSessionThread(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, _ := prepareTransportIntegrationSession(t, store, "concurrent-thread-binding")
	if _, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
		SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
		FromID: "second", Input: "continue",
	}); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("deliveries=%+v err=%v", deliveries, err)
	}
	for _, delivery := range deliveries {
		if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, delivery := range deliveries {
		go func() {
			<-start
			results <- store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", fmt.Sprintf("thread-%d", i), fmt.Sprintf("turn-%d", i), "completed")
		}()
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("want one accepted binding: %v / %v", first, second)
	}
	bound, err := store.Session(ctx, session.ID)
	if err != nil || bound.Harness != "codex" || bound.ThreadID == "" {
		t.Fatalf("Session binding=%+v err=%v", bound, err)
	}
	deliveries, err = store.Deliveries(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		run := delivery.AgentRun
		if run.State == core.AgentRunCompleted {
			if run.ThreadID != bound.ThreadID {
				t.Fatalf("accepted run disagrees with Session: %+v", run)
			}
			if err := store.BindAgentRun(ctx, run.ID, run.Harness, run.ThreadID, run.TurnID, "completed"); err != nil {
				t.Fatalf("binding replay: %v", err)
			}
		} else if run.State != core.AgentRunSubmitting || run.ThreadID != "" || run.TurnID != "" {
			t.Fatalf("rejected binding partially committed: %+v", run)
		}
	}
}

func TestEarlyDirectFollowNoThreadPredecessorRules(t *testing.T) {
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
			session, created, err := admitDirectFixture(t, store, ctx, directSessionInput(
				fmt.Sprintf("early-no-thread-%s-%d", strings.ReplaceAll(test.name, " ", "-"), time.Now().UnixNano()),
			))
			if err != nil || !created {
				t.Fatalf("admit Session created=%t err=%v", created, err)
			}
			deliveries, err := store.Deliveries(ctx, session.ID)
			if err != nil || len(deliveries) != 1 {
				t.Fatalf("initial deliveries=%#v err=%v", deliveries, err)
			}
			first := deliveries[0]
			if err := test.settleFirst(ctx, store, first.AgentRun); err != nil {
				t.Fatal(err)
			}
			follow, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
				SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
				FromID: "accepted-follow", Input: "continue after the predecessor",
			})
			if err != nil || !follow.Created {
				t.Fatalf("admit follow=%#v err=%v", follow, err)
			}
			selected, err := store.AgentMessage(ctx, session.ID)
			if err != nil || selected == nil {
				t.Fatalf("selected=%#v err=%v", selected, err)
			}
			wantMessageID := follow.Message.ID
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
	session, threadID := prepareTransportIntegrationSession(t, store, "submitting-follow-recovery")
	delivery, err := nextDelivery(ctx, store, session.ID)
	if err != nil || delivery == nil {
		t.Fatalf("initial delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil || !slices.ContainsFunc(deliveries, func(candidate core.Delivery) bool {
		return candidate.AgentRun.ID == delivery.AgentRun.ID && candidate.AgentRun.BaselineRecorded && candidate.AgentRun.BaselineTurnID == ""
	}) {
		t.Fatalf("prepared Delivery baseline=%#v err=%v", deliveries, err)
	}
	later, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "later-follow", Input: "must wait for recovery", Intent: core.MessageAuto, RefreshSkills: true})
	if err != nil || !later.Created || later.Message.Intent != core.MessageFollow {
		t.Fatalf("later Follow=%#v err=%v", later, err)
	}

	candidate, err := nextDelivery(ctx, store, session.ID)
	if err != nil || candidate == nil || candidate.AgentRun.ID != delivery.AgentRun.ID || candidate.AgentRun.State != core.AgentRunSubmitting {
		t.Fatalf("submitting candidate=%#v err=%v", candidate, err)
	}
	retry, err := nextDelivery(ctx, store, session.ID)
	if err != nil || retry == nil || retry.AgentRun.ID != delivery.AgentRun.ID || retry.AgentRun.State != core.AgentRunSubmitting {
		t.Fatalf("submitting retry=%#v err=%v", retry, err)
	}
	if err := store.BindAgentRun(ctx, retry.AgentRun.ID, "codex", threadID, "turn-recovered-"+session.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if next, err := nextDelivery(ctx, store, session.ID); err != nil || next == nil || next.Message.ID != later.Message.ID {
		t.Fatalf("next candidate=%#v err=%v, want later Follow", next, err)
	}
}

func TestSubmittingSteerRemainsPriorityDeliveryUntilReconciled(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, threadID := prepareTransportIntegrationSession(t, store, "submitting-steer-recovery")
	target, err := nextDelivery(ctx, store, session.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + session.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "recover-submitting-steer", Input: "adjust the active Turn", Intent: core.MessageSteer})
	if err != nil || !steer.Created {
		t.Fatalf("steer=%#v err=%v", steer, err)
	}
	selected, err := nextDelivery(ctx, store, session.ID)
	if err != nil || selected == nil || selected.Message.ID != steer.Message.ID {
		t.Fatalf("selected steer=%#v err=%v", selected, err)
	}
	if err := store.PrepareAgentRun(ctx, selected.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}
	if admitted, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: "human", FromID: "queued-after-steer", Input: "run after the active Turn"}); err != nil || !admitted.Created {
		t.Fatalf("queued Follow admitted=%#v err=%v", admitted, err)
	}

	for _, load := range []struct {
		name string
		fn   func() (*core.Delivery, error)
	}{
		{name: "first reload", fn: func() (*core.Delivery, error) { return nextDelivery(ctx, store, session.ID) }},
		{name: "second reload", fn: func() (*core.Delivery, error) { return nextDelivery(ctx, store, session.ID) }},
	} {
		candidate, err := load.fn()
		if err != nil || candidate == nil || candidate.Message.ID != steer.Message.ID || candidate.AgentRun.State != core.AgentRunSubmitting {
			t.Fatalf("%s submitting steer candidate=%#v err=%v", load.name, candidate, err)
		}
	}
}

func prepareTransportIntegrationSession(t *testing.T, store postgres.Store, label string) (core.Session, string) {
	t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	admitted, created, err := admitDirectFixture(t, store, ctx, directSessionInput(key))
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", admitted, created, err)
	}
	session, err := store.Session(ctx, admitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	threadID := "thread-" + session.ID
	return session, threadID
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func actionIntegrationSession(t *testing.T, suffix string) (*sql.DB, postgres.Store, core.Session) {
	t.Helper()
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	session, created, err := admitDirectFixture(t, store, ctx, directSessionInput(
		fmt.Sprintf("action-%s-%d", suffix, time.Now().UnixNano()),
	))
	if err != nil || !created {
		t.Fatalf("admit Session=%#v created=%t err=%v", session, created, err)
	}
	return db, store, session
}

func TestSandboxActionAttentionPersistsAcrossRetryAndClearsOnSuccess(t *testing.T) {
	_, store, session := actionIntegrationSession(t, "attention-recovery")
	ctx := context.Background()
	sandboxID := core.MainSandboxName(session.ID)
	actionID := core.ScopedActionID(session.ID, core.ActionRouteCreate, sandboxID)
	externals := &integrationExternals{}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
	client := newFaultClient(t, store, "dorf-action-attention-"+session.ID)
	const taskName = "dorf-action-attention-v1"
	attempts := 0
	externals.routeCreate = func() error {
		attempts++
		if attempts == 1 {
			return actionAttentionError(`provider route does not currently advertise model "missing-model"`)
		}
		return nil
	}
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if err := service.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, core.ActionSandboxCreate); err != nil {
			return core.TaskResultV1{}, err
		}
		err := service.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, core.ActionRouteCreate)
		return core.TaskResultV1{SessionID: session.ID, Outcome: "route-ready"}, err
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))
	spawned, err := client.Spawn(ctx, taskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{
		IdempotencyKey: taskName + ":" + session.ID,
		MaxAttempts:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSessionTask(ctx, session.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "action-attention-first", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	failed, err := client.FetchTaskResult(ctx, client.QueueName(), spawned.TaskID)
	if err != nil || failed == nil || failed.State != absurd.TaskFailed {
		t.Fatalf("failed task=%#v err=%v", failed, err)
	}
	attention, err := store.Session(ctx, session.ID)
	if err != nil || attention.WorkflowAttention != `provider route does not currently advertise model "missing-model"` || attention.WorkflowAttentionSource != actionID || attention.WorkflowAttentionAt.IsZero() {
		t.Fatalf("durable Action attention Session=%#v err=%v", attention, err)
	}
	unsettled, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionRouteCreate)
	if err != nil || unsettled.State != core.ActionUnsettled {
		t.Fatalf("unsettled route Action=%#v err=%v", unsettled, err)
	}

	if _, err := (core.Application{Store: store, Tasks: client}).RetryFailedSession(ctx, session.ID, "action-attention-retry-"+session.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "action-attention-retry", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Session(ctx, session.ID)
	if err != nil || recovered.WorkflowAttention != "" || recovered.WorkflowAttentionSource != "" || !recovered.WorkflowAttentionAt.IsZero() {
		t.Fatalf("recovered Session retained Action attention: Session=%#v err=%v", recovered, err)
	}
	settled, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionRouteCreate)
	if err != nil || settled.State != core.ActionSucceeded || attempts != 2 {
		t.Fatalf("recovered route Action=%#v attempts=%d err=%v", settled, attempts, err)
	}
}

func TestSandboxCleanupRequiresRouteRevoke(t *testing.T) {
	_, store, session := actionIntegrationSession(t, "cleanup-order")
	ctx := context.Background()
	sandboxID := core.MainSandboxName(session.ID)
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
	client := newFaultClient(t, store, "dorf-authority-"+session.ID)
	taskName := "dorf-authority-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if err := service.ExecuteSandboxAction(taskCtx, "wrong-session", sandboxID, create.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("wrong Session selected a provider mutation")
		}
		if err := service.ExecuteSandboxAction(taskCtx, session.ID, "wrong-sandbox", create.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("wrong Sandbox selected a provider mutation")
		}
		if err := service.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, revoke.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("route revoke reached provider before cleanup scheduling")
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "authority-refused"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSessionTask(ctx, session.ID, "", spawned.TaskID, taskName); err != nil {
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
	client.MustRegister(absurd.Task(recoveryTaskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if err := recovery.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, create.Kind); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "provider-reconciled"}, nil
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))
	recoveryTask, err := client.Spawn(ctx, recoveryTaskName, core.SessionTaskParams{SessionID: session.ID, PreviousTaskID: spawned.TaskID}, absurd.SpawnOptions{IdempotencyKey: recoveryTaskName + ":" + session.ID, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachSessionTask(ctx, session.ID, spawned.TaskID, recoveryTask.TaskID, recoveryTaskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "lost-provider-receipt", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	unsettled, err := store.GetOrCreateSandboxAction(ctx, sandboxID, core.ActionSandboxCreate)
	if err != nil || unsettled.State != core.ActionUnsettled || len(externals.effectKinds()) != 1 {
		t.Fatalf("lost provider receipt action=%#v effects=%v err=%v", unsettled, externals.effectKinds(), err)
	}
	if _, err := (core.Application{Store: store, Tasks: client}).RetryFailedSession(ctx, session.ID, "lost-provider-receipt-retry-"+session.ID); err != nil {
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
	_, store, session := actionIntegrationSession(t, "delete-before-revoke")
	ctx := context.Background()
	owned, err := store.Sandbox(ctx, core.MainSandboxName(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	remove, err := store.GetOrCreateSandboxAction(ctx, owned.ID, core.ActionSandboxDelete)
	if err != nil {
		t.Fatal(err)
	}
	externals := &integrationExternals{}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
	client := newFaultClient(t, store, "dorf-delete-before-revoke-"+session.ID)
	client.MustRegister(absurd.Task(core.CleanupTaskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		cleaning, err := store.Session(taskCtx, session.ID)
		if err != nil {
			return core.TaskResultV1{}, err
		}
		if err := service.ExecuteSandboxAction(taskCtx, cleaning.ID, owned.ID, remove.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("Sandbox delete reached provider before route revoke")
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "delete-refused"}, nil
	}))
	if err := requestCleanupFixture(ctx, store, session.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, core.CleanupTaskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: "delete-before-revoke:" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, session.ID, "", spawned.TaskID, core.CleanupTaskName); err != nil {
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
	session, threadID := prepareTransportIntegrationSession(t, store, "cleanup-accepted-steer")
	target, err := nextDelivery(ctx, store, session.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-cleanup-steer-" + session.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
		SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
		FromID: "cleanup-accepted-steer", Input: "accepted before cleanup", Intent: core.MessageSteer,
	})
	if err != nil || !steer.Created {
		t.Fatalf("steer=%#v err=%v", steer, err)
	}
	steerDelivery, err := nextDelivery(ctx, store, session.ID)
	if err != nil || steerDelivery == nil || steerDelivery.Message.ID != steer.Message.ID {
		t.Fatalf("steer delivery=%#v err=%v", steerDelivery, err)
	}
	if err := store.PrepareAgentRun(ctx, steerDelivery.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}

	sandboxID := core.MainSandboxName(session.ID)
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
	client := newFaultClient(t, store, "dorf-cleanup-accepted-steer-"+session.ID)
	client.MustRegister(absurd.Task(core.CleanupTaskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if _, _, err := service.PrepareCleanup(taskCtx, session.ID); err == nil || !strings.Contains(err.Error(), "remain") {
			return core.TaskResultV1{}, fmt.Errorf("cleanup did not retain active accepted steer: %v", err)
		}
		if err := service.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, revoke.Kind); err == nil || !strings.Contains(err.Error(), "Harness mutations remain unsettled") {
			return core.TaskResultV1{}, fmt.Errorf("route revoke did not enforce Harness barrier: %v", err)
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "accepted-steer-retained"}, nil
	}))
	if err := requestCleanupFixture(ctx, store, session.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, core.CleanupTaskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: "cleanup-accepted-steer:" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, session.ID, session.CurrentTaskID, spawned.TaskID, core.CleanupTaskName); err != nil {
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
	unsettled, err := store.UnsettledAgentMessages(ctx, session.ID)
	if err != nil || len(unsettled) != 1 || unsettled[0].MessageID != target.Message.ID {
		t.Fatalf("retained active target=%#v err=%v", unsettled, err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var settledSteer core.AgentRun
	for _, delivery := range deliveries {
		if delivery.Message.ID == steer.Message.ID {
			settledSteer = delivery.AgentRun
		}
	}
	if settledSteer.State != core.AgentRunCompleted || settledSteer.TurnID != targetTurnID {
		t.Fatalf("accepted steer was not settled from observation: %#v", settledSteer)
	}
}

func TestClosedAdmissionCleanupRecoversOrdinaryDirectRunWithoutExecutionEligibility(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, threadID := prepareTransportIntegrationSession(t, store, "cleanup-ordinary-closed-admission")
	delivery, err := nextDelivery(ctx, store, session.ID)
	if err != nil || delivery == nil {
		t.Fatalf("ordinary delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	turnID := "turn-cleanup-ordinary-" + session.ID
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", threadID, turnID, "running"); err != nil {
		t.Fatal(err)
	}

	sandboxID := core.MainSandboxName(session.ID)
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
	client := newFaultClient(t, store, "dorf-cleanup-ordinary-closed-"+session.ID)
	client.MustRegister(absurd.Task(core.CleanupTaskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		cleaning, sandboxes, err := service.PrepareCleanup(taskCtx, session.ID)
		if err != nil || cleaning.AdmissionOpen || len(sandboxes) != 1 {
			return core.TaskResultV1{}, fmt.Errorf("prepare closed ordinary cleanup: Session=%#v Sandboxes=%#v: %w", cleaning, sandboxes, err)
		}
		if err := service.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, revoke.Kind); err != nil {
			return core.TaskResultV1{}, err
		}
		if err := service.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, remove.Kind); err != nil {
			return core.TaskResultV1{}, err
		}
		if err := service.CompleteCleanup(taskCtx, session.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "ordinary-cleanup-complete"}, nil
	}))
	if err := requestCleanupFixture(ctx, store, session.ID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, core.CleanupTaskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: "cleanup-ordinary-closed:" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, session.ID, session.CurrentTaskID, spawned.TaskID, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "cleanup-ordinary-closed", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Session(ctx, session.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("cleanup Session=%#v err=%v", cleaned, err)
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
	db, _, session := actionIntegrationSession(t, "kind-grammar")
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
			insert into dorf.actions(id,session_id,kind,state,scope_key)
			values($1,$2,$3,'unsettled',$4)`, fmt.Sprintf("action-kind-%d-%s", i, session.ID), session.ID, test.kind, fmt.Sprintf("scope-%d", i))
		if accepted := err == nil; accepted != test.valid {
			t.Errorf("Action kind %q accepted=%t, want %t: %v", test.kind, accepted, test.valid, err)
		}
	}
}

func completeNextIntegrationRun(t *testing.T, store postgres.Store, sessionID, threadID, turnID string) core.AgentRun {
	t.Helper()
	delivery, err := nextDelivery(context.Background(), store, sessionID)
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
	routeCreate     func() error
	mu              sync.Mutex
	turns           []core.HarnessTurn
	submitted       []int64
	inputs          []string
	effects         []core.ActionKind
	turnStatus      string
	initialStarts   int
	steerErr        error
	terminalOnSteer bool
	startOnSteer    bool
}

type integrationAgentExecution struct {
	store     postgres.Store
	externals *integrationExternals
}

func (s integrationAgentExecution) ResolveAgentPrompt(_ context.Context, execution core.AgentMessageExecution) (string, error) {
	return execution.Message.Input, nil
}

func (s integrationAgentExecution) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	return integrationAgentOperation{externals: s.externals, execution: execution}, nil
}

// resultBoundaryAgentExecution lets Core's generic selector replay a completed
// steer receipt and exercise both completed-run result branches.
type resultBoundaryAgentExecution struct {
	externals *integrationExternals
	operation core.AgentRunOperation
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

func (s *cleanupOnlyAgentExecution) ResolveAgentPrompt(context.Context, core.AgentMessageExecution) (string, error) {
	s.executionCalls++
	return "", errors.New("ordinary prompt resolution must not run after admission closes")
}

func (s *cleanupOnlyAgentExecution) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	if execution.Session.AdmissionOpen {
		s.executionCalls++
		return nil, errors.New("ordinary execution Harness selection unexpectedly ran")
	}
	s.cleanupCalls++
	if execution.AgentRun.Role != "direct" {
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
		o.externals.initialStarts++
		if len(o.externals.turns) == 0 {
			turn := core.HarnessTurn{ID: "integration-turn-" + o.execution.Message.ID, Status: status}
			o.externals.submitted = append(o.externals.submitted, o.execution.Message.Sequence)
			o.externals.inputs = append(o.externals.inputs, input)
			o.externals.turns = append(o.externals.turns, turn)
		}
		return core.HarnessBinding{Harness: "codex", ThreadID: "integration-thread-" + o.execution.Session.ID, Turn: o.externals.turns[0]}, nil
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
	return core.HarnessBinding{Harness: "codex", ThreadID: "integration-thread-" + o.execution.Session.ID, Turn: o.externals.turns[len(o.externals.turns)-1]}, nil
}
func (o integrationAgentOperation) History(_ context.Context, run core.AgentRun) (core.HarnessHistory, error) {
	o.externals.mu.Lock()
	defer o.externals.mu.Unlock()
	threadID := run.ThreadID
	if threadID == "" {
		threadID = "integration-thread-" + o.execution.Session.ID
	}
	return core.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: append([]core.HarnessTurn(nil), o.externals.turns...)}, nil
}

func (e *integrationExternals) effect(kind core.ActionKind) error {
	e.mu.Lock()
	e.effects = append(e.effects, kind)
	e.mu.Unlock()
	return nil
}
func (e *integrationExternals) SandboxCreate(_ context.Context, _ core.Session, owned core.Sandbox) (string, error) {
	return owned.ID, e.effect(core.ActionSandboxCreate)
}
func (e *integrationExternals) RouteCreate(context.Context, core.Session, core.Sandbox, core.Route) error {
	if e.routeCreate != nil {
		return e.routeCreate()
	}
	return e.effect(core.ActionRouteCreate)
}
func (e *integrationExternals) SteerHistory(_ context.Context, _ core.Session, _ string, threadID string) (core.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return core.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: append([]core.HarnessTurn(nil), e.turns...)}, nil
}
func (e *integrationExternals) AgentSteer(_ context.Context, _ core.Session, delivery core.Delivery) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.submitted = append(e.submitted, delivery.Message.Sequence)
	if e.terminalOnSteer {
		for index := range e.turns {
			if e.turns[index].ID == delivery.Message.TargetTurnID {
				e.turns[index].Status = "completed"
			}
		}
	}
	if e.startOnSteer {
		e.turns = append(e.turns, core.HarnessTurn{ID: "event-follow", Status: "running", AcceptedMessageIDs: []string{delivery.AgentRun.ID}})
	}
	return delivery.Message.TargetTurnID, e.steerErr
}
func (e *integrationExternals) RouteRevoke(context.Context, core.Session, core.Sandbox, core.Route) error {
	return e.effect(core.ActionRouteRevoke)
}
func (e *integrationExternals) SandboxDelete(context.Context, core.Session, core.Sandbox) error {
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

func TestSessionTaskAttachmentFencesStaleEffectsAndAgentSelection(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	input := directSessionInput("reattach-cas-" + suffix)
	session, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("initial delivery=%#v err=%v", deliveries, err)
	}
	follow, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
		SessionID: session.ID, SandboxID: core.MainSandboxName(session.ID), FromKind: core.MessageFromHuman,
		FromID: "stale-task-early-follow", Input: "preserve this accepted follow",
	})
	if err != nil || !follow.Created {
		t.Fatalf("early follow=%#v err=%v", follow, err)
	}
	if err := store.PrepareAgentRun(ctx, deliveries[0].AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, deliveries[0].AgentRun.ID, "codex", "thread-authoritative", "turn-initial", "completed"); err != nil {
		t.Fatal(err)
	}
	owned, err := store.Sandbox(ctx, core.MainSandboxName(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	action, err := store.GetOrCreateSandboxAction(ctx, owned.ID, core.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	externals := &integrationExternals{}
	agents := integrationAgentExecution{store: store, externals: externals}
	service := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).WithAgentExecution(agents)
	staleTaskName := "stale-provider-effect-v1"
	client.MustRegister(absurd.Task(staleTaskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if _, err := service.ReconcileSessionAgent(taskCtx, session.ID); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("unattached task reached Agent Message selection")
		}
		if err := service.ExecuteSandboxAction(taskCtx, session.ID, owned.ID, action.Kind); err == nil {
			return core.TaskResultV1{}, fmt.Errorf("unattached task acquired provider authority")
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "stale-refused"}, nil
	}))
	_, err = client.Spawn(ctx, staleTaskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: staleTaskName + ":" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := client.Spawn(ctx, "unrelated-task", core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{QueueName: client.QueueName(), IdempotencyKey: "unrelated:" + session.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), client.QueueName(), unrelated.TaskID) })
	if err := store.AttachSessionTask(ctx, session.ID, "", unrelated.TaskID, "unrelated-task"); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "stale-effect-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := externals.effectKinds(); len(got) != 0 {
		t.Fatalf("unattached stale task mutated provider: %v", got)
	}
	unchanged, err := store.AgentMessageExecution(ctx, follow.Message.ID)
	if err != nil || unchanged.AgentRun.ThreadID != "" {
		t.Fatalf("stale task changed early follow Thread binding: %#v err=%v", unchanged.AgentRun, err)
	}
	if err := client.CancelTask(ctx, client.QueueName(), unrelated.TaskID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, direct.TaskName, core.SessionTaskParams{SessionID: session.ID, PreviousTaskID: ""}, absurd.SpawnOptions{IdempotencyKey: direct.TaskKey(session.ID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), client.QueueName(), spawned.TaskID) })
	if err := store.AttachSessionTask(ctx, session.ID, "", spawned.TaskID, direct.TaskName); err == nil {
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

func TestPostgresSessionFenceSerializesOverlappingClaims(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	firstEntered := make(chan struct{})
	release := make(chan struct{})
	secondEntered := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		errs <- store.WithSessionFence(ctx, "job-fence-integration", func() error { close(firstEntered); <-release; return nil })
	}()
	<-firstEntered
	go func() {
		errs <- store.WithSessionFence(ctx, "job-fence-integration", func() error { close(secondEntered); return nil })
	}()
	select {
	case <-secondEntered:
		close(release)
		t.Fatal("second claim crossed the PostgreSQL Session execution fence")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCleanupCompletesWithExplanatoryWorkflowAttention(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, _ := prepareTransportIntegrationSession(t, store, "cleanup-attention")
	if err := store.SetWorkflowAttention(ctx, session.ID, "operator:test", "explanatory only"); err != nil {
		t.Fatal(err)
	}
	deliveries, err := store.Deliveries(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, delivery := range deliveries {
		if err := store.InterruptAgentRun(ctx, delivery.AgentRun.ID, "cleanup"); err != nil {
			t.Fatal(err)
		}
	}
	if err := requestCleanupFixture(ctx, store, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachCleanupTask(ctx, session.ID, session.CurrentTaskID, "cleanup-task-"+session.ID, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	sandboxID := core.MainSandboxName(session.ID)
	for _, kind := range []core.ActionKind{core.ActionRouteRevoke, core.ActionSandboxDelete} {
		action, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CompleteCleanup(ctx, session.ID, "cleanup-task-"+session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteCleanup(ctx, session.ID, "cleanup-task-"+session.ID); err != nil {
		t.Fatalf("exact cleanup completion replay failed: %v", err)
	}
	if err := store.CompleteCleanup(ctx, session.ID, "other-cleanup-task"); err == nil {
		t.Fatal("foreign cleanup task replay was accepted")
	}
	cleaned, err := store.Session(ctx, session.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete || cleaned.WorkflowAttention != "" || cleaned.WorkflowAttentionSource != "" || !cleaned.WorkflowAttentionAt.IsZero() {
		t.Fatalf("cleanup terminal retained explanatory attention: session=%#v err=%v", cleaned, err)
	}
}
