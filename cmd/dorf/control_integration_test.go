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

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// The response is deliberately discarded after the handler commits, matching
// a client that cannot know whether its first request succeeded.

// Replay must use the retained profile and AI connection even when the
// deployment defaults can no longer be consulted.

// Decode into a fresh DTO because omitted optional fields must not reuse a previous value.

// Recreate the API and Absurd client again: cleanup remains one durable
// request and one attached task rather than being rescheduled.

func TestControlAPISessionListKeepsKeysetContinuity(t *testing.T) {
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
		id string
		at time.Time
	}
	fixtures := []listedFixture{
		{base + "-z", tiedAt},
		{base + "-y", tiedAt},
		{base + "-x", tiedAt.Add(-time.Second)},
		{base + "-w", tiedAt.Add(-2 * time.Second)},
	}
	insert := func(fixture listedFixture) {
		t.Helper()
		_, err := store.DB.ExecContext(ctx, `
insert into dorf.sessions(
    id,admission_key,
    sandbox_profile,sandbox_profile_revision,provider_connection,model,reasoning_effort,admitted_at
) values($1,$2,$3,(select candidate_revision from dorf.sandbox_profiles where name=$3),'primary','model-test','high',$4)
`, fixture.id, "admission-"+fixture.id, profileName, fixture.at)
		if err != nil {
			t.Fatalf("insert Session list fixture %s: %v", fixture.id, err)
		}
	}
	for _, fixture := range fixtures {
		insert(fixture)
	}
	t.Cleanup(func() {
		for _, fixture := range fixtures {
			if _, err := store.DB.ExecContext(context.Background(), `delete from dorf.sessions where id=$1`, fixture.id); err != nil {
				t.Errorf("delete Session list fixture %s: %v", fixture.id, err)
			}
		}
	})

	handler := controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth, controlAPISessions{store: store}, controlAPIProfiles{store: store}).Handler
	firstResponse := controlTestRequest(t, handler, http.MethodGet, "/v1/sessions?limit=2", credential, "", nil)
	var first controlapi.SessionList
	controlTestJSON(t, firstResponse, http.StatusOK, &first)
	if len(first.Sessions) != 2 || first.Sessions[0].ID != fixtures[0].id ||
		first.Sessions[1].ID != fixtures[1].id || first.NextCursor == nil {
		t.Fatalf("first Session page=%#v", first)
	}

	newer := listedFixture{base + "-new", tiedAt.Add(3 * time.Second)}
	fixtures = append(fixtures, newer)
	insert(newer)
	secondResponse := controlTestRequest(t, handler, http.MethodGet,
		"/v1/sessions?limit=2&cursor="+url.QueryEscape(*first.NextCursor), credential, "", nil)
	var second controlapi.SessionList
	controlTestJSON(t, secondResponse, http.StatusOK, &second)
	if len(second.Sessions) != 2 || second.Sessions[0].ID != fixtures[2].id ||
		second.Sessions[1].ID != fixtures[3].id {
		t.Fatalf("second Session page=%#v", second)
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
		"/v1/sessions?cursor="+url.QueryEscape(tamperedCursor), credential, "", nil)
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

func controlTestHandler(store postgres.Store, tasks *absurd.Client, provider gateway.Gateway, auth controlauth.Service, runtimes core.SandboxRuntimeResolver) http.Handler {
	queueName := config.QueueName
	if tasks != nil {
		queueName = tasks.QueueName()
	}
	reader := controlreader.Service{Store: store, Runtimes: runtimes, Provider: provider}
	return controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth,
		controlAPISessions{
			store: store, tasks: tasks,
			directAdmissions: direct.NewAdmissionService(store, queueName, reader),
			reader:           reader,
		}, controlAPIProfiles{store: store}).Handler
}

type controlTestRuntimes struct {
	profile  string
	contents []byte
}

func (r controlTestRuntimes) ResolveSandbox(_ context.Context, profile core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if profile.Name != r.profile {
		return core.SandboxRuntime{}, fmt.Errorf("unexpected Sandbox profile %q", profile)
	}
	return core.SandboxRuntime{SandboxProfile: profile, Files: r}, nil
}

func (r controlTestRuntimes) ReadSandboxFile(context.Context, core.Session, core.Sandbox, string) ([]byte, error) {
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
