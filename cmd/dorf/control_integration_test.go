package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestControlAPIPostgresReplayRestartAndCleanup(t *testing.T) {
	ctx := context.Background()
	store, firstTasks, profileName := controlTestStore(t)
	provider := controlTestGateway(t)
	auth := controlauth.Service{Store: store}
	enrollment, err := auth.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	fileBytes := []byte{0, 1, '\n', 255}
	runtimes := controlTestRuntimes{profile: profileName, contents: fileBytes}
	first := controlTestHandler(store, firstTasks, provider, auth, runtimes, blob.Store{Root: t.TempDir()})
	redeem := controlTestRequest(t, first, http.MethodPost, "/v1/auth/enrollments/redeem", "", "", controlapi.RedeemRequest{
		EnrollmentCode: enrollment.Token, ClientName: profileName, Credential: credential,
	})
	if redeem.Code != http.StatusCreated {
		t.Fatalf("redeem status=%d body=%s", redeem.Code, redeem.Body.String())
	}

	key := fmt.Sprintf("control-api-replay-%d", time.Now().UnixNano())
	input := controlapi.AdmitJobRequest{Goal: "prove remote durable replay", AIConnection: "primary", Model: "model-test", Reasoning: "high"}
	// The response is deliberately discarded after the handler commits, matching
	// a client that cannot know whether its first request succeeded.
	lost := controlTestRequest(t, first, http.MethodPost, "/v1/jobs", credential, key, input)
	if lost.Code != http.StatusCreated {
		t.Fatalf("first admission status=%d body=%s", lost.Code, lost.Body.String())
	}
	committed, err := store.Job(ctx, core.JobID(key))
	if err != nil || committed.CurrentTaskID == "" || committed.ProviderConnection != input.AIConnection {
		t.Fatalf("committed Job=%#v err=%v", committed, err)
	}
	// Replay must use the retained profile and AI connection even when the
	// deployment defaults can no longer be consulted.
	if err := os.Remove(filepath.Join(provider.StatePath, "connections.json")); err != nil {
		t.Fatal(err)
	}

	restartedTasks := controlTestTasks(t, store.DB, firstTasks.QueueName(), false)
	restarted := controlTestHandler(store, restartedTasks, provider, controlauth.Service{Store: store}, runtimes, blob.Store{Root: t.TempDir()})
	replay := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, input)
	var replayed controlapi.DirectJob
	controlTestJSON(t, replay, http.StatusOK, &replayed)
	afterReplay, err := store.Job(ctx, committed.ID)
	if err != nil || replayed.ID != committed.ID || replayed.InitialMessageID == "" || afterReplay.CurrentTaskID != committed.CurrentTaskID {
		t.Fatalf("replay Job=%#v durable=%#v err=%v", replayed, afterReplay, err)
	}
	var problem controlapi.Problem

	messageKey := fmt.Sprintf("control-message-%d", time.Now().UnixNano())
	messageInput := controlapi.SendMessageRequest{Text: "continue before the initial Turn settles", Intent: "follow"}
	early := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey, messageInput)
	var accepted controlapi.Message
	controlTestJSON(t, early, http.StatusCreated, &accepted)
	if accepted.JobID != committed.ID || accepted.Sequence != 2 || accepted.Delivery.State != "accepted" {
		t.Fatalf("early Message=%#v", accepted)
	}
	replayedMessage := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey, messageInput)
	var sameMessage controlapi.Message
	controlTestJSON(t, replayedMessage, http.StatusOK, &sameMessage)
	if sameMessage.ID != accepted.ID {
		t.Fatalf("replayed Message=%#v want ID %s", sameMessage, accepted.ID)
	}
	changedMessage := messageInput
	changedMessage.Text = "different input"
	messageConflict := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/messages", credential, messageKey, changedMessage)
	controlTestJSON(t, messageConflict, http.StatusConflict, &problem)
	if problem.Code != "idempotency_conflict" {
		t.Fatalf("Message conflict=%#v", problem)
	}

	file := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/"+replayed.Sandboxes[0].ID+"/files?path=result.bin", credential, "", nil)
	if file.Code != http.StatusOK || !bytes.Equal(file.Body.Bytes(), fileBytes) {
		t.Fatalf("Sandbox file status=%d bytes=%v", file.Code, file.Body.Bytes())
	}
	invalidFile := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/"+replayed.Sandboxes[0].ID+"/files?path=../secret", credential, "", nil)
	controlTestJSON(t, invalidFile, http.StatusUnprocessableEntity, &problem)
	if problem.Code != "invalid_file_path" {
		t.Fatalf("invalid file problem=%#v", problem)
	}
	missingFile := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/missing-sandbox/files?path=result.bin", credential, "", nil)
	controlTestJSON(t, missingFile, http.StatusNotFound, &problem)
	if problem.Code != "sandbox_not_found" {
		t.Fatalf("missing Sandbox problem=%#v", problem)
	}
	evidence := controlTestRequest(t, restarted, http.MethodGet, "/v1/jobs/"+committed.ID+"/evidence", credential, "", nil)
	var retained controlapi.EvidenceList
	controlTestJSON(t, evidence, http.StatusOK, &retained)
	if retained.Evidence == nil || len(retained.Evidence) != 0 {
		t.Fatalf("direct Job Evidence=%#v, want an explicit empty collection", retained)
	}

	retryKey := fmt.Sprintf("control-retry-%d", time.Now().UnixNano())
	notEligible := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+committed.ID+"/retries", credential, retryKey, nil)
	controlTestJSON(t, notEligible, http.StatusConflict, &problem)
	if problem.Code != "retry_unavailable" {
		t.Fatalf("retry problem=%#v", problem)
	}

	changed := input
	changed.Goal = "different input must conflict"
	conflict := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, changed)
	controlTestJSON(t, conflict, http.StatusConflict, &problem)
	if problem.Code != "idempotency_conflict" {
		t.Fatalf("conflict=%#v", problem)
	}

	cleanup := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+committed.ID+"/cleanup", credential, "", nil)
	var cleaning controlapi.DirectJob
	controlTestJSON(t, cleanup, http.StatusOK, &cleaning)
	cleaningFact, err := store.Job(ctx, committed.ID)
	if err != nil || cleaning.Admission.Open || cleaning.Cleanup.State != "running" || cleaningFact.CurrentTaskID == committed.CurrentTaskID {
		t.Fatalf("cleanup view=%#v durable=%#v err=%v", cleaning, cleaningFact, err)
	}
	fencedFile := controlTestRequest(t, restarted, http.MethodGet, "/v1/sandboxes/"+replayed.Sandboxes[0].ID+"/files?path=result.bin", credential, "", nil)
	controlTestJSON(t, fencedFile, http.StatusConflict, &problem)
	if problem.Code != "file_unavailable" {
		t.Fatalf("cleanup-fenced file problem=%#v", problem)
	}

	// Recreate the API and Absurd client again: cleanup remains one durable
	// request and one attached task rather than being rescheduled.
	finalTasks := controlTestTasks(t, store.DB, firstTasks.QueueName(), false)
	finalHandler := controlTestHandler(store, finalTasks, provider, controlauth.Service{Store: store}, runtimes, blob.Store{Root: t.TempDir()})
	repeated := controlTestRequest(t, finalHandler, http.MethodPut, "/v1/jobs/"+committed.ID+"/cleanup", credential, "", nil)
	controlTestJSON(t, repeated, http.StatusOK, &cleaning)
	finalFact, err := store.Job(ctx, committed.ID)
	if err != nil || finalFact.CurrentTaskID != cleaningFact.CurrentTaskID || finalFact.CleanupState != core.CleanupScheduled {
		t.Fatalf("replayed cleanup durable=%#v err=%v", finalFact, err)
	}
}

func TestControlAPIWorkflowAdmissionsProjectAndReplay(t *testing.T) {
	ctx := context.Background()
	store, tasks, profileName := controlTestStore(t)
	provider := controlTestGateway(t)
	auth := controlauth.Service{Store: store}
	enrollment, err := auth.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	installations := &controlTestGitHub{installation: "42"}
	handler := controlTestHandlerWithGitHub(store, tasks, provider, auth,
		controlTestRuntimes{profile: profileName}, blob.Store{Root: t.TempDir()}, installations)
	redeem := controlTestRequest(t, handler, http.MethodPost, "/v1/auth/enrollments/redeem", "", "", controlapi.RedeemRequest{
		EnrollmentCode: enrollment.Token, ClientName: profileName, Credential: credential,
	})
	if redeem.Code != http.StatusCreated {
		t.Fatalf("redeem status=%d body=%s", redeem.Code, redeem.Body.String())
	}

	codingKey := fmt.Sprintf("control-coding-%d", time.Now().UnixNano())
	codingInput := controlapi.AdmitCodingJobRequest{
		Goal: "  preserve this exact coding goal\n", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("a", 40), BaseBranch: "main", Profile: profileName, AIConnection: "primary", Model: "model-test",
	}
	codingResponse := controlTestRequest(t, handler, http.MethodPost, "/v1/workflows/coding/jobs", credential, codingKey, codingInput)
	var codingJob controlapi.CodingJob
	controlTestJSON(t, codingResponse, http.StatusCreated, &codingJob)
	if codingJob.Kind != controlapi.JobKindCoding || codingJob.Goal != codingInput.Goal ||
		codingJob.Branch != "dorf/"+core.JobID(codingKey) || codingJob.StartingRevision != codingInput.Revision ||
		codingJob.Revision != codingInput.Revision || codingJob.WorkflowRevision == "" || codingJob.Outcome != nil {
		t.Fatalf("coding Job=%#v", codingJob)
	}
	if installations.calls != 1 {
		t.Fatalf("coding installation discoveries=%d, want 1", installations.calls)
	}
	codingFact, err := store.Job(ctx, codingJob.ID)
	if err != nil || codingFact.CurrentTaskID == "" || codingFact.ProviderConnection != codingInput.AIConnection {
		t.Fatalf("coding durable Job=%#v err=%v", codingFact, err)
	}

	// A restarted API must replay the retained GitHub installation without
	// consulting current external discovery.
	restartedTasks := controlTestTasks(t, store.DB, tasks.QueueName(), false)
	unavailableGitHub := &controlTestGitHub{err: fmt.Errorf("GitHub must not be consulted during replay")}
	restarted := controlTestHandlerWithGitHub(store, restartedTasks, provider, controlauth.Service{Store: store},
		controlTestRuntimes{profile: profileName}, blob.Store{Root: t.TempDir()}, unavailableGitHub)
	replayCoding := controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/coding/jobs", credential, codingKey, codingInput)
	var sameCoding controlapi.CodingJob
	controlTestJSON(t, replayCoding, http.StatusOK, &sameCoding)
	replayedCodingFact, replayedCodingErr := store.Job(ctx, codingJob.ID)
	if sameCoding.ID != codingJob.ID || unavailableGitHub.calls != 0 || replayedCodingErr != nil || replayedCodingFact.CurrentTaskID != codingFact.CurrentTaskID {
		t.Fatalf("coding replay=%#v GitHub calls=%d durable=%#v err=%v", sameCoding, unavailableGitHub.calls, replayedCodingFact, replayedCodingErr)
	}
	changedCoding := codingInput
	changedCoding.BaseBranch = "develop"
	var codingConflict controlapi.Problem
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/coding/jobs", credential, codingKey, changedCoding), http.StatusConflict, &codingConflict)
	if codingConflict.Code != "idempotency_conflict" {
		t.Fatalf("coding replay conflict=%#v", codingConflict)
	}

	investigationKey := fmt.Sprintf("control-investigation-%d", time.Now().UnixNano())
	investigationInput := controlapi.AdmitInvestigationJobRequest{
		Brief: "  preserve this exact investigation brief\n", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("b", 40), Profile: profileName, AIConnection: "primary", Model: "model-test",
	}
	investigationResponse := controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/codebase-investigation/jobs", credential, investigationKey, investigationInput)
	var investigationJob controlapi.InvestigationJob
	controlTestJSON(t, investigationResponse, http.StatusCreated, &investigationJob)
	if investigationJob.Kind != controlapi.JobKindInvestigation || investigationJob.Goal != investigationInput.Brief ||
		investigationJob.Source.Repository != investigationInput.Repository || investigationJob.Source.Revision != investigationInput.Revision ||
		investigationJob.Report.Path != "REPORT.md" || investigationJob.Report.SandboxID == "" {
		t.Fatalf("investigation Job=%#v", investigationJob)
	}
	replayInvestigation := controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/codebase-investigation/jobs", credential, investigationKey, investigationInput)
	var sameInvestigation controlapi.InvestigationJob
	controlTestJSON(t, replayInvestigation, http.StatusOK, &sameInvestigation)
	if sameInvestigation.ID != investigationJob.ID {
		t.Fatalf("investigation replay=%#v", sameInvestigation)
	}
	foreignKind := codingInput
	foreignKind.Profile = "missing-profile-must-not-be-resolved"
	foreignKindConflict := controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/coding/jobs", credential, investigationKey, foreignKind)
	var foreignKindProblem controlapi.Problem
	controlTestJSON(t, foreignKindConflict, http.StatusConflict, &foreignKindProblem)
	if foreignKindProblem.Code != "idempotency_conflict" || unavailableGitHub.calls != 0 {
		t.Fatalf("foreign-kind replay conflict=%#v GitHub calls=%d", foreignKindProblem, unavailableGitHub.calls)
	}
	changedInvestigation := investigationInput
	changedInvestigation.Revision = strings.Repeat("c", 40)
	var investigationConflict controlapi.Problem
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPost, "/v1/workflows/codebase-investigation/jobs", credential, investigationKey, changedInvestigation), http.StatusConflict, &investigationConflict)
	if investigationConflict.Code != "idempotency_conflict" {
		t.Fatalf("investigation replay conflict=%#v", investigationConflict)
	}

	message := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs/"+codingJob.ID+"/messages", credential,
		"message-"+codingJob.ID, controlapi.SendMessageRequest{Text: "continue", Intent: "follow"})
	var accepted controlapi.Message
	controlTestJSON(t, message, http.StatusCreated, &accepted)
	if accepted.JobID != codingJob.ID || accepted.Sequence != 2 {
		t.Fatalf("workflow Message=%#v", accepted)
	}

	completedCodingResponse := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+codingJob.ID+"/abandon", credential, "", nil)
	var completedCoding controlapi.CodingJob
	controlTestJSON(t, completedCodingResponse, http.StatusOK, &completedCoding)
	if completedCoding.Execution.State != "complete" || completedCoding.Cleanup.State != "running" || completedCoding.Outcome == nil || completedCoding.Outcome.Kind != string(coding.OutcomeAbandoned) {
		t.Fatalf("completed coding Job=%#v", completedCoding)
	}
	var replayedAbandon controlapi.CodingJob
	controlTestJSON(t, controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+codingJob.ID+"/abandon", credential, "", nil), http.StatusOK, &replayedAbandon)
	if replayedAbandon.Outcome == nil || replayedAbandon.Outcome.Kind != string(coding.OutcomeAbandoned) {
		t.Fatalf("replayed abandon=%#v", replayedAbandon)
	}
	cleanupInvestigation := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+investigationJob.ID+"/cleanup", credential, "", nil)
	var cleaningInvestigation controlapi.InvestigationJob
	controlTestJSON(t, cleanupInvestigation, http.StatusOK, &cleaningInvestigation)
	if cleaningInvestigation.Cleanup.State != "running" || cleaningInvestigation.Execution.State != "stopped" {
		t.Fatalf("cleanup-fenced investigation Job=%#v", cleaningInvestigation)
	}
}

func TestControlAPIJobListKeepsKeysetContinuity(t *testing.T) {
	ctx := context.Background()
	store, _, profileName := controlTestStore(t)
	auth := controlauth.Service{Store: store}
	enrollment, err := auth.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := auth.Redeem(ctx, enrollment.Token, "pagination-client", credential); err != nil || !created {
		t.Fatalf("redeem pagination Client: created=%t err=%v", created, err)
	}

	base := fmt.Sprintf("job-page-%d", time.Now().UnixNano())
	tiedAt := time.Now().UTC().AddDate(100, 0, 0).Truncate(time.Microsecond)
	type listedFixture struct {
		id       string
		workflow string
		revision string
		source   bool
		at       time.Time
	}
	fixtures := []listedFixture{
		{base + "-z", "", "", false, tiedAt},
		{base + "-y", string(coding.Workflow), coding.WorkflowRevision, false, tiedAt},
		{base + "-x", string(investigation.Workflow), investigation.WorkflowRevision, true, tiedAt.Add(-time.Second)},
		{base + "-w", "", "", false, tiedAt.Add(-2 * time.Second)},
		// A retained but unrecognized workflow revision must not consume a page slot.
		{base + "-unsupported", string(coding.Workflow), "unrecognized", false, tiedAt.Add(time.Second)},
	}
	insert := func(fixture listedFixture) {
		t.Helper()
		_, err := store.DB.ExecContext(ctx, `
insert into dorf.jobs(
    id,admission_key,workflow_name,workflow_revision,goal,
    sandbox_profile,provider_connection,model,reasoning_effort,admitted_at
) values($1,$2,$3,$4,'pagination fixture',$5,'primary','model-test','high',$6)
`, fixture.id, "admission-"+fixture.id, fixture.workflow, fixture.revision, profileName, fixture.at)
		if err != nil {
			t.Fatalf("insert Job list fixture %s: %v", fixture.id, err)
		}
		if fixture.source {
			_, err = store.DB.ExecContext(ctx, `
insert into dorf.codebase_investigation_sources(job_id,workflow_name,repository,revision)
values($1,$2,'https://github.com/aphronio/dorf.git',$3)
`, fixture.id, string(investigation.Workflow), strings.Repeat("a", 40))
		}
		if err != nil {
			t.Fatalf("insert Job list source fixture %s: %v", fixture.id, err)
		}
	}
	for _, fixture := range fixtures {
		insert(fixture)
	}
	t.Cleanup(func() {
		for _, fixture := range fixtures {
			if _, err := store.DB.ExecContext(context.Background(), `delete from dorf.codebase_investigation_sources where job_id=$1`, fixture.id); err != nil {
				t.Errorf("delete Job list source fixture %s: %v", fixture.id, err)
			}
			if _, err := store.DB.ExecContext(context.Background(), `delete from dorf.jobs where id=$1`, fixture.id); err != nil {
				t.Errorf("delete Job list fixture %s: %v", fixture.id, err)
			}
		}
	})

	handler := controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth, controlAPIJobs{store: store}).Handler
	unsupported := controlTestRequest(t, handler, http.MethodGet, "/v1/jobs/"+fixtures[4].id, credential, "", nil)
	var unsupportedProblem controlapi.Problem
	controlTestJSON(t, unsupported, http.StatusNotFound, &unsupportedProblem)
	if unsupportedProblem.Code != "job_not_found" {
		t.Fatalf("unsupported-revision Job Problem=%#v", unsupportedProblem)
	}
	firstResponse := controlTestRequest(t, handler, http.MethodGet, "/v1/jobs?limit=2", credential, "", nil)
	var first controlapi.JobList
	controlTestJSON(t, firstResponse, http.StatusOK, &first)
	if len(first.Jobs) != 2 || first.Jobs[0].ID != fixtures[0].id || first.Jobs[0].Kind != controlapi.JobKindDirect ||
		first.Jobs[1].ID != fixtures[1].id || first.Jobs[1].Kind != controlapi.JobKindCoding || first.NextCursor == nil {
		t.Fatalf("first Job page=%#v", first)
	}

	newer := listedFixture{base + "-new", "", "", false, tiedAt.Add(3 * time.Second)}
	fixtures = append(fixtures, newer)
	insert(newer)
	secondResponse := controlTestRequest(t, handler, http.MethodGet,
		"/v1/jobs?limit=2&cursor="+url.QueryEscape(*first.NextCursor), credential, "", nil)
	var second controlapi.JobList
	controlTestJSON(t, secondResponse, http.StatusOK, &second)
	if len(second.Jobs) != 2 || second.Jobs[0].ID != fixtures[2].id || second.Jobs[0].Kind != controlapi.JobKindInvestigation ||
		second.Jobs[1].ID != fixtures[3].id || second.Jobs[1].Kind != controlapi.JobKindDirect {
		t.Fatalf("second Job page=%#v", second)
	}

	payload, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	payload = bytes.Replace(payload, []byte(`"v":1`), []byte(`"v":2`), 1)
	tamperedCursor := base64.RawURLEncoding.EncodeToString(payload)
	if tamperedCursor == *first.NextCursor {
		t.Fatal("cursor version tamper did not change the token")
	}
	tampered := controlTestRequest(t, handler, http.MethodGet,
		"/v1/jobs?cursor="+url.QueryEscape(tamperedCursor), credential, "", nil)
	var problem controlapi.Problem
	controlTestJSON(t, tampered, http.StatusBadRequest, &problem)
	if problem.Code != "invalid_cursor" {
		t.Fatalf("tampered cursor Problem=%#v", problem)
	}
}

func controlTestStore(t *testing.T) (postgres.Store, *absurd.Client, string) {
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
	suffix := time.Now().UnixNano()
	profileName := fmt.Sprintf("control-api-%d", suffix)
	incusAuthorityHash, err := (deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}).AuthorityHash()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	profile, _, err := store.CreateSandboxProfile(context.Background(), core.SandboxProfile{
		Name: profileName, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("a", 64),
		IncusEndpointAuthorityHash: incusAuthorityHash, IncusProject: "dorf", IncusStoragePool: "default",
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB", IncusGatewayURL: "http://10.44.0.1:8317/v1",
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(context.Background(), profile.Name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(context.Background(), verification, "codex-test")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(context.Background(), verification)
	}
	if err == nil {
		_, err = store.SetDefaultSandboxProfile(context.Background(), profile.Name)
	}
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	queue := fmt.Sprintf("%s_control_%d", config.QueueName, suffix)
	tasks := controlTestTasks(t, db, queue, true)
	t.Cleanup(func() {
		if err := tasks.DropQueue(context.Background(), queue); err != nil {
			t.Errorf("drop test queue %q: %v", queue, err)
		}
		db.Close()
	})
	return store, tasks, profileName
}

func controlTestTasks(t *testing.T, db *sql.DB, queue string, create bool) *absurd.Client {
	t.Helper()
	tasks, err := absurd.New(absurd.Options{DB: db, QueueName: queue})
	if err == nil && create {
		err = tasks.CreateQueue(context.Background(), queue)
	}
	if err != nil {
		t.Fatal(err)
	}
	return tasks
}

func controlTestGateway(t *testing.T) gateway.Gateway {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "model-test"}}})
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"credentials/openai-0123456789abcdef.key": "unused-test-secret\n",
		"connections.json":                        `[{"name":"primary","provider":"openai","auth_mode":"api_key","credential_ref":"openai-0123456789abcdef.key","default":true}]`,
		"authority.json":                          `{"guard_key":"guard-test","management_key":"management-test"}`,
		"broker.yaml":                             fmt.Sprintf("host: %q\nport: %s\n", parsed.Hostname(), parsed.Port()),
	}
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(state, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return gateway.Gateway{StatePath: state, Client: server.Client()}
}

func controlTestHandler(store postgres.Store, tasks *absurd.Client, provider gateway.Gateway, auth controlauth.Service, runtimes core.SandboxRuntimeResolver, evidence blob.Store) http.Handler {
	return controlTestHandlerWithGitHub(store, tasks, provider, auth, runtimes, evidence,
		&controlTestGitHub{installation: "42"})
}

func controlTestHandlerWithGitHub(store postgres.Store, tasks *absurd.Client, provider gateway.Gateway, auth controlauth.Service, runtimes core.SandboxRuntimeResolver, evidence blob.Store, github coding.InstallationDiscovery) http.Handler {
	application := coreApplication(store, tasks)
	reader := controlreader.Service{Store: store, Runtimes: runtimes, Provider: provider, Installations: github}
	return controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth,
		controlAPIJobs{
			store: store, tasks: tasks,
			directAdmissions:        direct.NewAdmissionService(store, application, reader),
			codingAdmissions:        coding.NewAdmissionService(store, application, reader, reader),
			investigationAdmissions: investigation.NewAdmissionService(store, application, reader),
			reader:                  reader, evidence: evidence,
		}).Handler
}

type controlTestGitHub struct {
	installation string
	err          error
	calls        int
}

func (g *controlTestGitHub) DiscoverInstallation(context.Context, string) (string, error) {
	g.calls++
	return g.installation, g.err
}

type controlTestRuntimes struct {
	profile  string
	contents []byte
}

func (r controlTestRuntimes) ResolveSandbox(_ context.Context, profile string) (core.SandboxRuntime, error) {
	if profile != r.profile {
		return core.SandboxRuntime{}, fmt.Errorf("unexpected Sandbox profile %q", profile)
	}
	return core.SandboxRuntime{SandboxProfile: r.profile, Files: r}, nil
}

func (r controlTestRuntimes) ReadSandboxFile(context.Context, core.Job, core.Sandbox, string) ([]byte, error) {
	return append([]byte(nil), r.contents...), nil
}

func controlTestRequest(t *testing.T, handler http.Handler, method, path, credential, key string, input any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &body)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func controlTestJSON(t *testing.T, response *httptest.ResponseRecorder, status int, output any) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), output); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
}
