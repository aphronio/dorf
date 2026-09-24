package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
)

func TestWorkspaceInspectionUsesLiveWorkerFactsWithoutNativeExecution(t *testing.T) {
	ctx := context.Background()
	store, tasks, name := controlTestStore(t)
	profile, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name + "-cloud", Provider: core.SandboxProviderE2B, Harness: "codex",
		Artifact: "test:exact-build", E2BGatewayURL: "https://models.example.test/v1", E2BSandboxTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, profile.Name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex-test")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := store.AdmitDirect(ctx, core.SessionAdmission{AdmissionKey: profile.Name, SandboxProfile: profile.Name, ProviderConnection: "primary", Model: "test", ReasoningEffort: "high"}, tasks.QueueName())
	if err != nil {
		t.Fatal(err)
	}
	backup := validCheckpointConfig()
	backup.ProfileName, backup.ProfileRevision = profile.Name, "*"
	file := writeCheckpointConfig(t, backup, 0600)
	cfg := config.Config{Workspace: "/workspace/selected", E2BAPIKey: "synthetic", PersistenceFile: file}
	resolver := profileRuntimeResolver{cfg: cfg, store: store}
	privateHandler, err := controlreader.NewHandler(strings.Repeat("a", 64), controlreader.Service{Store: store, Workspace: resolver.workspace})
	if err != nil {
		t.Fatal(err)
	}
	private := httptest.NewServer(privateHandler)
	defer private.Close()
	reader, err := controlreader.NewClient(private.URL, strings.Repeat("a", 64), private.Client())
	if err != nil {
		t.Fatal(err)
	}
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = auth.IssueKey(ctx, "workspace-test", credential); err != nil {
		t.Fatal(err)
	}
	handler := controlapi.NewServer(controlapi.Discovery{}, auth, controlAPISessions{store: store, tasks: tasks, reader: reader}, nil).Handler
	path := "/v1/sessions/" + session.ID + "/workspace"
	read := func() persistence.Workspace {
		t.Helper()
		response := controlTestRequest(t, handler, http.MethodGet, path, credential, "", nil)
		var result persistence.Workspace
		controlTestJSON(t, response, 200, &result)
		if strings.Contains(response.Body.String(), ".codex") || strings.Contains(response.Body.String(), backup.SecretAccessKey) {
			t.Fatal("private persistence details exposed")
		}
		return result
	}
	first := read()
	if first.Path != cfg.Workspace || !first.BackupEnabled || first.IdleDelaySeconds != nil || first.LastSuccessfulCheckpointAt != nil {
		t.Fatalf("first observation: %#v", first)
	}
	// Populate a synthetic published receipt; this test observes existing facts,
	// while the persistence suite proves capture and fenced publication.
	_, err = store.DB.ExecContext(ctx, `insert into dorf.sandbox_checkpoints(repository,snapshot_id,sandbox_id,resource_id,profile_name,profile_revision,last_activity_at,native_revision,delivery_hold_count,cleanup)
 select $1,$2,s.id,s.active_resource_id,$3,$4,clock_timestamp(),0,0,false from dorf.sandboxes s where s.id=$5`, session.ID, strings.Repeat("b", 64), profile.Name, profile.DefinitionHash, core.MainSandboxName(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	second := read()
	if second.LastSuccessfulCheckpointAt == nil || !second.ObservedAt.After(first.ObservedAt) {
		t.Fatalf("fresh checkpoint missing: %#v", second)
	}
	backup.ProfileName = "another-profile"
	contents, err := os.ReadFile(writeCheckpointConfig(t, backup, 0600))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(file, contents, 0600); err != nil {
		t.Fatal(err)
	}
	disabled := read()
	if disabled.BackupEnabled || disabled.IdleDelaySeconds != nil || disabled.LastSuccessfulCheckpointAt == nil {
		t.Fatalf("disabled coverage lost history: %#v", disabled)
	}
	before, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = read()
	after, err := store.Session(ctx, session.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("inspection changed execution state")
	}
	if got := controlTestRequest(t, handler, http.MethodGet, path, "invalid", "", nil); got.Code != 401 {
		t.Fatalf("unauthenticated status=%d", got.Code)
	}
	if err = os.WriteFile(file, []byte(`{"invalid": "private-value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	failed := controlTestRequest(t, handler, http.MethodGet, path, credential, "", nil)
	if failed.Code < 500 || strings.Contains(failed.Body.String(), "private-value") {
		t.Fatalf("unavailable response=%d %s", failed.Code, failed.Body.String())
	}
}
