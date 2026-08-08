package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

func publicationInput(key string) postgres.NewJob {
	return postgres.NewJob{
		AdmissionKey: key, Goal: "publish one exact Revision", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("a", 40), Branch: "dorf/issue-43", ProviderConnection: "primary",
		Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf",
		GitHubInstallation: "42", BaseBranch: "greenfield",
	}
}

func TestPostgresPublicationIdentityFreshnessAndSamePullRequestRefresh(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	input := publicationInput(fmt.Sprintf("publication-integration-%d", time.Now().UnixNano()))
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set workflow_phase='ready' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	job, push, pull, err := store.BeginPublication(ctx, job.ID, input.Revision)
	if err != nil || job.WorkflowPhase != "publishing" {
		t.Fatalf("begin=%#v push=%#v pull=%#v err=%v", job, push, pull, err)
	}
	if push.ID != spine.ScopedActionID(job.ID, spine.ActionRepositoryPush, input.Revision) || pull.ID != spine.ScopedActionID(job.ID, spine.ActionGitHubPullRequest, input.Revision) {
		t.Fatal("publication Actions do not have stable Job/Revision identities")
	}
	_, repeatedPush, repeatedPull, err := store.BeginPublication(ctx, job.ID, input.Revision)
	if err != nil || repeatedPush.ID != push.ID || repeatedPull.ID != pull.ID {
		t.Fatalf("repeated identities push=%#v pull=%#v err=%v", repeatedPush, repeatedPull, err)
	}
	if err := store.RecordPush(ctx, push.ID, input.Revision); err != nil {
		t.Fatal(err)
	}
	digest1 := strings.Repeat("1", 64)
	proposal := spine.GitHubProposal{JobID: job.ID, Repository: input.GitHubRepository, InstallationID: input.GitHubInstallation, BaseBranch: input.BaseBranch, HeadBranch: input.Branch, Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", ProposedRevision: input.Revision, ObservedRemoteHead: input.Revision, BodyDigest: digest1}
	if err := store.RecordProposal(ctx, pull.ID, proposal); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Proposal(ctx, job.ID)
	if err != nil || stored == nil || stored.Stale || stored.Number != 43 || stored.BodyDigest != digest1 {
		t.Fatalf("proposal=%#v err=%v", stored, err)
	}
	idempotent, samePush, samePull, err := store.BeginPublication(ctx, job.ID, input.Revision)
	if err != nil || idempotent.WorkflowPhase != "published" || samePush.ID != push.ID || samePull.ID != pull.ID {
		t.Fatalf("published idempotency job=%#v push=%#v pull=%#v err=%v", idempotent, samePush, samePull, err)
	}

	later := strings.Repeat("b", 40)
	if _, err := db.ExecContext(ctx, `insert into dorf.revisions(job_id,oid,parent_oid,branch,generation) values($1,$2,$3,$4,1)`, job.ID, later, input.Revision, input.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.jobs set revision=$2,workflow_phase='ready' where id=$1`, job.ID, later); err != nil {
		t.Fatal(err)
	}
	stale, err := store.Proposal(ctx, job.ID)
	if err != nil || stale == nil || !stale.Stale {
		t.Fatalf("later Revision reused stale proposal proof: %#v err=%v", stale, err)
	}
	_, laterPush, laterPull, err := store.BeginPublication(ctx, job.ID, later)
	if err != nil || laterPush.ID == push.ID || laterPull.ID == pull.ID {
		t.Fatalf("later Actions push=%#v pull=%#v err=%v", laterPush, laterPull, err)
	}
	if err := store.RecordPush(ctx, laterPush.ID, later); err != nil {
		t.Fatal(err)
	}
	digest2 := strings.Repeat("2", 64)
	proposal.ProposedRevision, proposal.ObservedRemoteHead, proposal.BodyDigest = later, later, digest2
	if err := store.RecordProposal(ctx, laterPull.ID, proposal); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.Proposal(ctx, job.ID)
	if err != nil || refreshed == nil || refreshed.Stale || refreshed.Number != 43 || refreshed.ProposedRevision != later || refreshed.BodyDigest != digest2 {
		t.Fatalf("same-PR refresh=%#v err=%v", refreshed, err)
	}
}

func TestPostgresFreshAdmissionRequiresCompleteGitHubAuthorityAndSchemaHasNoCredentialColumns(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	input := publicationInput(fmt.Sprintf("publication-admission-%d", time.Now().UnixNano()))
	input.BaseBranch = ""
	if _, _, err := store.Admit(ctx, input); err == nil || !strings.Contains(err.Error(), "explicit base branch") {
		t.Fatalf("missing base admission error=%v", err)
	}
	input = publicationInput(fmt.Sprintf("publication-scope-%d", time.Now().UnixNano()))
	input.GitHubInstallation = ""
	if _, _, err := store.Admit(ctx, input); err == nil {
		t.Fatal("partial GitHub installation authority was admitted")
	}
	var secretColumns int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.columns where table_schema='dorf' and (column_name ~ '(github|access|installation)_token' or column_name ilike '%private_key%' or column_name ilike '%credential%' or column_name ilike '%password%')`).Scan(&secretColumns); err != nil {
		t.Fatal(err)
	}
	if secretColumns != 0 {
		t.Fatalf("Dorf PostgreSQL schema exposes %d credential-shaped columns", secretColumns)
	}
}

func TestPostgresGuardedDogfoodBindingIsNarrowImmutableAndIdempotent(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	jobID := fmt.Sprintf("job-dogfood-bind-%d", time.Now().UnixNano())
	revision := "a09e08da11cc89aacd8aee6d33a4a38c45d53824"
	branch := "dorf/issue-43-durable-github-publication"
	if _, err := db.ExecContext(ctx, `insert into dorf.jobs(id,admission_key,goal,repository,revision,starting_revision,branch,provider_connection,model,reasoning_effort) values($1,$2,'issue 43 dogfood','https://github.com/aphronio/dorf.git',$3,$3,$4,'primary','gpt-5.6-sol','high')`, jobID, jobID, revision, branch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.revisions(job_id,oid,branch,generation) values($1,$2,$3,0)`, jobID, revision, branch); err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindDogfoodPublicationAuthority(ctx, jobID, "aphronio/dorf", "42", "greenfield")
	if err != nil || bound.GitHubRepository != "aphronio/dorf" || bound.GitHubInstallation != "42" || bound.BaseBranch != "greenfield" {
		t.Fatalf("bound=%#v err=%v", bound, err)
	}
	repeated, err := store.BindDogfoodPublicationAuthority(ctx, jobID, "aphronio/dorf", "42", "greenfield")
	if err != nil || repeated.GitHubInstallation != "42" {
		t.Fatalf("idempotent bind=%#v err=%v", repeated, err)
	}
	if _, err := store.BindDogfoodPublicationAuthority(ctx, jobID, "aphronio/dorf", "43", "greenfield"); err == nil {
		t.Fatal("guarded dogfood authority was rebound")
	}
}
