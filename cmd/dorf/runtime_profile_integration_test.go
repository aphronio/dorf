package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

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
	if _, _, err := store.CreateSandboxProfile(ctx, core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderIncus, Harness: "codex", Artifact: strings.Repeat("e", 64),
		IncusNetwork: "incusbr0", IncusDiskSize: "40GiB",
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
		cfg:   config.Config{Workspace: "/workspace", BlobRoot: t.TempDir(), TurnTimeout: time.Minute},
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
