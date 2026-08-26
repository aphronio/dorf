package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestGuidedSelectedLegacyProfilesBindDefinitionsBeforeLiveReverification(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	incusName, e2bName := "legacy-incus-"+suffix, "legacy-e2b-"+suffix
	if _, err := db.ExecContext(ctx, `
insert into dorf.sandbox_profiles(name,provider,harness,artifact,incus_network,incus_disk_size)
values($1,'incus','codex',$2,'incusbr0','40GiB')`, incusName, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
insert into dorf.sandbox_profiles(
    name,provider,harness,artifact,e2b_gateway_url,e2b_sandbox_timeout_seconds,e2b_allow_internet
) values($1,'e2b','codex',$2,'https://gateway.example/v1',3300,false)`, e2bName, "dorf:legacy-build"); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{incusName, e2bName} {
		if _, err := db.ExecContext(ctx, `insert into dorf.sandbox_profile_verifications(
    profile_name,contract_version,sandbox_id,ownership_nonce,harness_version,probe_completed_at,cleaned_at
) values($1,$2,$3,$4,'legacy-codex',clock_timestamp(),clock_timestamp())`,
			name, core.BaseProfileContract, "sandbox-"+name, fmt.Sprintf("%064x", index+1)); err != nil {
			t.Fatal(err)
		}
	}
	incusProfile, err := store.SandboxProfile(ctx, incusName)
	if err != nil {
		t.Fatal(err)
	}
	e2bProfile, err := store.SandboxProfile(ctx, e2bName)
	if err != nil {
		t.Fatal(err)
	}
	plans := []guidedProfilePlan{
		{Provider: core.SandboxProviderIncus, Name: incusName, Harness: "codex", Existing: &incusProfile},
		{Provider: core.SandboxProviderE2B, Name: e2bName, Harness: "codex", Existing: &e2bProfile},
	}

	path := filepath.Join(t.TempDir(), "config", "deployment.json")
	if err := deployment.Save(path, deployment.Config{Database: deployment.Database{
		Host: "127.0.0.1", Port: 5432, Name: "dorf", User: "dorf", Password: "secret",
		Image: "postgres:17", ImageID: "sha256:test",
	}}); err != nil {
		t.Fatal(err)
	}
	authority := &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	cfg := &config.Config{DeploymentPath: path, Incus: authority}
	plans, err = bindGuidedLegacyProfileDefinitions(ctx, store, cfg, plans, "incusbr0", "10.44.0.1")
	if err != nil {
		t.Fatal(err)
	}
	storedDeployment, found, err := deployment.Load(path)
	if err != nil || !found || storedDeployment.Incus == nil || *storedDeployment.Incus != *authority {
		t.Fatalf("persisted authority found=%t config=%#v err=%v", found, storedDeployment.Incus, err)
	}
	for _, plan := range plans {
		profile := *plan.Existing
		if profile.DefinitionHash == "" || profile.DefinitionHash != profile.CurrentDefinitionHash() ||
			profile.Verification != nil || profile.BaseVerified() {
			t.Fatalf("upgraded profile=%#v", profile)
		}
	}
	upgradedIncus := *plans[0].Existing
	wantAuthority, err := authority.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	if upgradedIncus.IncusEndpointAuthorityHash != wantAuthority || upgradedIncus.IncusProject != incus.DefaultProject ||
		upgradedIncus.IncusStoragePool != incus.DefaultStoragePool || upgradedIncus.IncusGatewayURL != "http://10.44.0.1:8317/v1" {
		t.Fatalf("upgraded Incus definition=%#v", upgradedIncus)
	}
	upgradedE2B := *plans[1].Existing
	if upgradedE2B.Artifact != e2bProfile.Artifact || upgradedE2B.E2BGatewayURL != e2bProfile.E2BGatewayURL ||
		upgradedE2B.E2BSandboxTimeout != e2bProfile.E2BSandboxTimeout || upgradedE2B.E2BAllowInternet != e2bProfile.E2BAllowInternet {
		t.Fatalf("upgraded E2B definition changed semantics: before=%#v after=%#v", e2bProfile, upgradedE2B)
	}

	for _, plan := range plans {
		_, verification, err := store.BeginSandboxProfileVerification(ctx, plan.Name)
		if err == nil {
			err = store.RecordSandboxProfileProbe(ctx, verification, "codex live")
		}
		if err == nil {
			err = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	replayed, err := bindGuidedLegacyProfileDefinitions(ctx, store, cfg, plans, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range replayed {
		profile, err := store.SandboxProfile(ctx, plan.Name)
		if err != nil || !profile.BaseVerified() {
			t.Fatalf("replayed profile=%#v err=%v", profile, err)
		}
	}
}

func TestGuidedLegacyIncusBindingPersistsDeploymentBeforeProfileAdoption(t *testing.T) {
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
	name := fmt.Sprintf("legacy-order-%d", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, `
insert into dorf.sandbox_profiles(name,provider,harness,artifact,incus_network,incus_disk_size)
values($1,'incus','codex',$2,'incusbr0','40GiB')`, name, strings.Repeat("4", 64)); err != nil {
		t.Fatal(err)
	}
	legacy, err := store.SandboxProfile(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	plans := []guidedProfilePlan{{
		Provider: core.SandboxProviderIncus, Name: name, Harness: "codex", Existing: &legacy,
	}}
	cfg := &config.Config{
		DeploymentPath: filepath.Join(t.TempDir(), "missing", "deployment.json"),
		Incus:          &deployment.Incus{Endpoint: "unix:///var/lib/incus/unix.socket"},
	}
	if _, err := bindGuidedLegacyProfileDefinitions(ctx, store, cfg, plans, "incusbr0", "10.44.0.1"); err == nil {
		t.Fatal("legacy profile was adopted before Deployment authority persistence")
	}
	retained, err := store.SandboxProfile(ctx, name)
	if err != nil || retained.DefinitionHash != "" || retained.IncusEndpointAuthorityHash != "" {
		t.Fatalf("retained legacy profile=%#v err=%v", retained, err)
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
