package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestGuidedLocalIncusRejectsPublicGatewayBeforeAuthorityRetention(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := postgres.Store{DB: db}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	path := setupTestDeploymentPath(t)
	authority := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	cfg := config.Config{DeploymentPath: path, Incus: authority}
	_, err = prepareGuidedSetup(
		ctx,
		store,
		&cfg,
		setupOptions{Yes: true, ProfileName: fmt.Sprintf("reject-local-route-%d", time.Now().UnixNano()), GatewayURL: "https://models.dorf.example/v1"},
		[]core.SandboxProvider{core.SandboxProviderIncus},
		newSetupPresenter(io.Discard),
		io.Discard,
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "require E2B or a remote HTTPS Incus authority") {
		t.Fatalf("local public Gateway rejection error=%v", err)
	}
	stored, found, loadErr := deployment.Load(path)
	if loadErr != nil || !found || stored.Incus != nil {
		t.Fatalf("rejected local setup retained authority=%#v found=%t error=%v", stored.Incus, found, loadErr)
	}
}

func TestGuidedSetupRetargetsAndInvalidatesTheExistingE2BProfile(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := postgres.Store{DB: db}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("setup-retarget-%d", time.Now().UnixNano())
	profile, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderE2B, Harness: "codex",
		Artifact: "dorf:exact-build", E2BGatewayURL: "https://dorf.example.test/v1",
		E2BSandboxTimeout: 55 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex-test")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err == nil {
		profile, err = store.SetDefaultSandboxProfile(ctx, name)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !profile.BaseVerified() || !profile.Default {
		t.Fatalf("prepared profile=%#v", profile)
	}
	resolved, err := resolveSetupSandboxOptions(ctx, store, config.Config{}, setupOptions{}, setupPresenter{output: io.Discard, interactive: true})
	if err != nil || len(resolved.SandboxProviders) != 1 || resolved.SandboxProviders[0] != profile.Provider ||
		resolved.ProfileName != profile.Name || resolved.Harness != profile.Harness {
		t.Fatalf("retained setup options=%#v error=%v", resolved, err)
	}

	profile, err = retargetGuidedE2BProfile(ctx, store, profile, "https://models.dorf.example.test/v1")
	if err != nil {
		t.Fatal(err)
	}
	if profile.E2BGatewayURL != "https://models.dorf.example.test/v1" || profile.BaseVerified() || profile.Default {
		t.Fatalf("retargeted profile=%#v", profile)
	}
}

func TestAdmittedJobRuntimeIgnoresLaterVerificationReceiptState(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := postgres.Store{DB: db}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("runtime-reverify-%d", time.Now().UnixNano())
	incusAuthority := deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	incusAuthorityHash, err := incusAuthority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("e", 64),
		IncusEndpointAuthorityHash: incusAuthorityHash, IncusProject: "dorf", IncusStoragePool: "default",
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB", IncusGatewayURL: "http://10.44.0.1:8317/v1",
	}); err != nil {
		t.Fatal(err)
	}
	_, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err == nil {
		err = store.RecordSandboxProfileProbe(ctx, verification, "codex runtime")
	}
	if err == nil {
		err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if err != nil {
		t.Fatal(err)
	}
	input := coding.Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: "runtime-reverify-" + name, Goal: "continue through profile re-verification",
			SandboxProfile: name, ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		},
		Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40), Branch: "dorf/runtime-reverify",
		GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield",
	}
	job, created, err := store.AdmitCoding(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit Job=%#v created=%v err=%v", job, created, err)
	}
	_, refreshing, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	resolver := profileRuntimeResolver{
		cfg:   config.Config{Workspace: "/workspace", BlobRoot: t.TempDir(), TurnTimeout: time.Minute, Incus: &incusAuthority},
		store: store,
	}
	assertRuntime := func(state string) {
		t.Helper()
		sandbox, err := resolver.ResolveSandbox(ctx, job.SandboxProfile)
		if err != nil || sandbox.SandboxProfile != name || sandbox.Execution == nil {
			t.Fatalf("%s Sandbox runtime=%#v err=%v", state, sandbox, err)
		}
		workflow, err := resolver.ResolveCoding(ctx, job.SandboxProfile)
		if err != nil || workflow.Profile.SandboxProfile != name || workflow.Agent == nil || workflow.Coding == nil {
			t.Fatalf("%s coding runtime=%#v err=%v", state, workflow, err)
		}
	}
	assertRuntime("pending verification")
	if err := store.RecordSandboxProfileVerificationError(ctx, refreshing, fmt.Errorf("verification provider unavailable")); err != nil {
		t.Fatal(err)
	}
	profile, err := store.SandboxProfile(ctx, name)
	if err != nil || profile.BaseVerified() || profile.Verification == nil || profile.Verification.LastError == "" {
		t.Fatalf("failed verification profile=%#v err=%v", profile, err)
	}
	assertRuntime("failed verification")
}
