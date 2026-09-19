package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"

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

func (p providerCheck) Check(context.Context, string) error { return p.err }

func (providerCheck) DefaultConnection() (string, error) { return "primary", nil }

func (providerCheck) DefaultModel(string) (string, error) { return "gpt-5.6-sol", nil }

type failOnceOperationBarrier struct {
	mu     sync.Mutex
	point  string
	failed bool
}

type actionAttentionError string

func (e actionAttentionError) Error() string       { return string(e) }
func (actionAttentionError) AttentionNeeded() bool { return true }

func (b *failOnceOperationBarrier) ReachOperation(_ context.Context, point, _, _ string) error {
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
	core.SessionReconciliation
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
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
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

func TestProvisionAndCleanupSerializeBothWinnerOrders(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	externals := &blockingCreateExternals{
		integrationExternals: &integrationExternals{}, entered: make(chan struct{}), release: make(chan struct{}),
	}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim)
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

func TestPostgresDirectAdmissionReplayRecoversTaskAttachment(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	input := core.SessionAdmission{
		AdmissionKey:   fmt.Sprintf("direct-%d", time.Now().UnixNano()),
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
	}
	session, created, err := admitDirectFixture(t, store, ctx, input)
	if err != nil || !created {
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
	if err != nil || stopped.ExecutionAttentionSource != source || stopped.ExecutionAttention != failure.Error() {
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

// Native acceptance survives a worker loss before the local binding is saved.

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
	if err := attachTaskFixture(store, ctx, session.ID, spawned.TaskID, taskName); err != nil {
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
	if err != nil || attention.ExecutionAttention != `provider route does not currently advertise model "missing-model"` || attention.ExecutionAttentionSource != actionID || attention.ExecutionAttentionAt.IsZero() {
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
	if err != nil || recovered.ExecutionAttention != "" || recovered.ExecutionAttentionSource != "" || !recovered.ExecutionAttentionAt.IsZero() {
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
	if err := attachTaskFixture(store, ctx, session.ID, spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "authority-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if got := externals.effectKinds(); len(got) != 0 {
		t.Fatalf("forged tuple or premature cleanup mutated provider: %v", got)
	}

	barrier := &failOnceOperationBarrier{point: core.BarrierSandboxCreated}
	recovery := core.NewExecutionService(store, externals, barrier, absurdruntime.RequireClaim)
	recoveryTaskName := "lost-provider-receipt-v1"
	client.MustRegister(absurd.Task(recoveryTaskName, func(taskCtx context.Context, _ core.SessionTaskParams) (core.TaskResultV1, error) {
		if err := recovery.ExecuteSandboxAction(taskCtx, session.ID, sandboxID, create.Kind); err != nil {
			return core.TaskResultV1{}, err
		}
		return core.TaskResultV1{SessionID: session.ID, Outcome: "provider-reconciled"}, nil
	}, absurd.TaskOptions{DefaultMaxAttempts: 1}))
	recoveryTask, err := client.Spawn(ctx, recoveryTaskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{IdempotencyKey: recoveryTaskName + ":" + session.ID, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := attachTaskFixture(store, ctx, session.ID, recoveryTask.TaskID, recoveryTaskName); err != nil {
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
	if err := attachTaskFixture(store, ctx, session.ID, spawned.TaskID, core.CleanupTaskName); err != nil {
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

// resultBoundaryAgentExecution lets Core's generic selector replay a completed
// steer receipt and exercise both completed-run result branches.

func (*integrationExternals) Harness() string { return "codex" }

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

func (e *integrationExternals) RouteRevoke(context.Context, core.Session, core.Sandbox, core.Route) error {
	return e.effect(core.ActionRouteRevoke)
}
func (e *integrationExternals) SandboxDelete(context.Context, core.Session, core.Sandbox) error {
	return e.effect(core.ActionSandboxDelete)
}

func (e *integrationExternals) effectKinds() []core.ActionKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]core.ActionKind(nil), e.effects...)
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

func TestCleanupCompletesWithExplanatoryExecutionAttention(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, _ := prepareTransportIntegrationSession(t, store, "cleanup-attention")
	if err := store.SetExecutionAttention(ctx, session.ID, "operator:test", "explanatory only"); err != nil {
		t.Fatal(err)
	}
	if err := requestCleanupFixture(ctx, store, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := attachTaskFixture(store, ctx, session.ID, "cleanup-task-"+session.ID, core.CleanupTaskName); err != nil {
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
	if err != nil || cleaned.CleanupState != core.CleanupComplete || cleaned.ExecutionAttention != "" || cleaned.ExecutionAttentionSource != "" || !cleaned.ExecutionAttentionAt.IsZero() {
		t.Fatalf("cleanup terminal retained explanatory attention: session=%#v err=%v", cleaned, err)
	}
}
