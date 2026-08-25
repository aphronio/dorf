package main

import (
	"bytes"
	"context"
	"database/sql"
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

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gateway"
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
	first := controlTestHandler(store, firstTasks, provider, auth)
	redeem := controlTestRequest(t, first, http.MethodPost, "/v1/auth/enrollments/redeem", "", "", controlapi.RedeemRequest{
		EnrollmentCode: enrollment.Token, ClientName: profileName, Credential: credential,
	})
	if redeem.Code != http.StatusCreated {
		t.Fatalf("redeem status=%d body=%s", redeem.Code, redeem.Body.String())
	}

	key := fmt.Sprintf("control-api-replay-%d", time.Now().UnixNano())
	input := controlapi.AdmitJobRequest{Goal: "prove remote durable replay", Profile: profileName, Model: "model-test", Reasoning: "high"}
	// The response is deliberately discarded after the handler commits, matching
	// a client that cannot know whether its first request succeeded.
	lost := controlTestRequest(t, first, http.MethodPost, "/v1/jobs", credential, key, input)
	if lost.Code != http.StatusCreated {
		t.Fatalf("first admission status=%d body=%s", lost.Code, lost.Body.String())
	}
	committed, err := store.Job(ctx, core.JobID(key))
	if err != nil || committed.CurrentTaskID == "" {
		t.Fatalf("committed Job=%#v err=%v", committed, err)
	}

	restartedTasks := controlTestTasks(t, store.DB, firstTasks.QueueName(), false)
	restarted := controlTestHandler(store, restartedTasks, provider, controlauth.Service{Store: store})
	replay := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, input)
	var replayed controlapi.Job
	controlTestJSON(t, replay, http.StatusOK, &replayed)
	afterReplay, err := store.Job(ctx, committed.ID)
	if err != nil || replayed.ID != committed.ID || afterReplay.CurrentTaskID != committed.CurrentTaskID {
		t.Fatalf("replay Job=%#v durable=%#v err=%v", replayed, afterReplay, err)
	}

	changed := input
	changed.Goal = "different input must conflict"
	conflict := controlTestRequest(t, restarted, http.MethodPost, "/v1/jobs", credential, key, changed)
	var problem controlapi.Problem
	controlTestJSON(t, conflict, http.StatusConflict, &problem)
	if problem.Code != "idempotency_conflict" {
		t.Fatalf("conflict=%#v", problem)
	}

	cleanup := controlTestRequest(t, restarted, http.MethodPut, "/v1/jobs/"+committed.ID+"/cleanup", credential, "", nil)
	var cleaning controlapi.Job
	controlTestJSON(t, cleanup, http.StatusOK, &cleaning)
	cleaningFact, err := store.Job(ctx, committed.ID)
	if err != nil || cleaning.Admission.Open || cleaning.Cleanup.State != "running" || cleaningFact.CurrentTaskID == committed.CurrentTaskID {
		t.Fatalf("cleanup view=%#v durable=%#v err=%v", cleaning, cleaningFact, err)
	}

	// Recreate the API and Absurd client again: cleanup remains one durable
	// request and one attached task rather than being rescheduled.
	finalTasks := controlTestTasks(t, store.DB, firstTasks.QueueName(), false)
	finalHandler := controlTestHandler(store, finalTasks, provider, controlauth.Service{Store: store})
	repeated := controlTestRequest(t, finalHandler, http.MethodPut, "/v1/jobs/"+committed.ID+"/cleanup", credential, "", nil)
	controlTestJSON(t, repeated, http.StatusOK, &cleaning)
	finalFact, err := store.Job(ctx, committed.ID)
	if err != nil || finalFact.CurrentTaskID != cleaningFact.CurrentTaskID || finalFact.CleanupState != core.CleanupScheduled {
		t.Fatalf("replayed cleanup durable=%#v err=%v", finalFact, err)
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
	profile, _, err := store.CreateSandboxProfile(context.Background(), core.SandboxProfile{
		Name: profileName, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("a", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
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

func controlTestHandler(store postgres.Store, tasks *absurd.Client, provider gateway.Gateway, auth controlauth.Service) http.Handler {
	return controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth,
		controlAPIJobs{store: store, tasks: tasks, gateway: provider}).Handler
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
