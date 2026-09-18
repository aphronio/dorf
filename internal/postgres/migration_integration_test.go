package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestCurrentBaselineInventory(t *testing.T) {
	baseline, err := migrationFiles.ReadFile("migrations/001_greenfield.sql")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(baseline)); got != "57ec6537431bb0484231b01f930d50dc1a835e4108329ce3020b7e7a86298948" {
		t.Fatalf("current 001_greenfield.sql checksum=%s", got)
	}
	files, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range files {
		if !file.IsDir() {
			names = append(names, file.Name())
		}
	}
	if !reflect.DeepEqual(names, dorfMigrations) {
		t.Fatalf("embedded migrations=%v execution order=%v", names, dorfMigrations)
	}
}

func TestCurrentBaselineReplaysIdempotentlyAndRejectsRetiredIdentity(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `select pg_advisory_xact_lock(hashtextextended('dorf-schema-baseline',0))`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `drop schema if exists dorf cascade`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
create schema dorf;
create table dorf.schema_migrations(name text primary key);
insert into dorf.schema_migrations(name) values ('001_baseline.sql')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateDorf(ctx, tx); err == nil || err.Error() != "existing Dorf schema has no baseline identity; recreate this prototype database" {
		t.Fatalf("retired baseline identity error=%v", err)
	}
	if _, err := tx.ExecContext(ctx, `drop schema dorf cascade`); err != nil {
		t.Fatal(err)
	}
	baseline, err := migrationFiles.ReadFile("migrations/001_greenfield.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(baseline)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `savepoint reject_incomplete_profile`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `insert into dorf.sandbox_profiles(name,provider,harness,artifact,incus_network,incus_disk_size) values('incomplete-profile','incus','codex',repeat('a',64),'incusbr0','40GiB')`); err == nil {
		t.Fatal("current schema accepted a Profile without definition and endpoint custody")
	}
	if _, err := tx.ExecContext(ctx, `rollback to savepoint reject_incomplete_profile`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `
insert into dorf.sandbox_profiles(name,provider,harness,artifact,definition_hash,incus_endpoint_authority_hash,incus_project,incus_storage_pool,incus_network,incus_disk_size,incus_gateway_url)
values('current-profile','incus','codex',repeat('a',64),repeat('b',64),repeat('c',64),'dorf','default','incusbr0','40GiB','http://10.44.0.1:8317/v1');
insert into dorf.jobs(id,admission_key,workflow_name,workflow_revision,goal,sandbox_profile,provider_connection,model,reasoning_effort)
values('job-current','current-admission','','','run direct caller intent','current-profile','primary','gpt-5.6-sol','high');
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input) values('message-current','job-current','human','dorf:initial',1,'run direct caller intent');
insert into dorf.sandboxes(id,job_id,name,ownership_nonce) values('sandbox-current','job-current','default',repeat('d',64));
insert into dorf.agent_runs(id,job_id,message_id,role,state,sandbox_id) values('run-current','job-current','message-current','direct','pending','sandbox-current')`); err != nil {
		t.Fatalf("current schema insert: %v", err)
	}
	var sandboxName string
	if err := tx.QueryRowContext(ctx, `select sandbox_name from dorf.review_run_projection where id='run-current'`).Scan(&sandboxName); err != nil || sandboxName != "default" {
		t.Fatalf("projected Sandbox name=%q err=%v", sandboxName, err)
	}
	var controlClients, retries, artifacts, drafts bool
	if err := tx.QueryRowContext(ctx, `select to_regclass('dorf.control_clients') is not null,to_regclass('dorf.job_retry_requests') is not null,to_regclass('dorf.artifacts') is not null,to_regclass('dorf.codebase_investigation_drafts') is not null`).Scan(&controlClients, &retries, &artifacts, &drafts); err != nil {
		t.Fatal(err)
	}
	if !controlClients || !retries || artifacts || drafts {
		t.Fatalf("control clients=%t retries=%t artifacts=%t drafts=%t", controlClients, retries, artifacts, drafts)
	}
	if _, err := tx.ExecContext(ctx, `
insert into dorf.jobs(id,admission_key,workflow_name,workflow_revision,goal,sandbox_profile,provider_connection,model,reasoning_effort)
values('job-retired','retired-admission','codebase-investigation','2','inspect source','current-profile','primary','model-test','high');
insert into dorf.jobs(id,admission_key,workflow_name,workflow_revision,goal,sandbox_profile,provider_connection,model,reasoning_effort)
values('job-coding-retired','coding-retired-admission','coding-to-proposal','1','edit source','current-profile','primary','model-test','high');
update dorf.jobs set admission_open=false,cleanup_state='complete',cleaned_at=clock_timestamp() where id='job-coding-retired';
insert into dorf.coding_to_proposal_inputs(job_id,workflow_name,repository,starting_revision,revision,branch,github_repository,github_installation_id,base_branch)
values('job-coding-retired','coding-to-proposal','https://example.test/source.git',repeat('a',40),repeat('a',40),'task/retired','example/source','42','main');
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input)
values('message-coding-retired','job-coding-retired','human','dorf:initial',1,'edit source');
update dorf.jobs set admission_open=false,cleanup_state='complete',cleaned_at=clock_timestamp() where id='job-retired';
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input)
values('message-retired','job-retired','human','dorf:initial',1,'inspect source');
insert into dorf.codebase_investigation_sources(job_id,workflow_name,repository,revision)
values('job-retired','codebase-investigation','https://example.test/source.git',repeat('a',40))`); err != nil {
		t.Fatal(err)
	}
	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatalf("baseline replay: %v", err)
	}
	var retiredInput string
	if err := tx.QueryRowContext(ctx, `select m.input
from dorf.jobs j join dorf.job_messages m on m.job_id=j.id where j.id='job-retired'`).Scan(&retiredInput); err != nil || retiredInput != "inspect source" {
		t.Fatalf("retirement changed retained input: input=%q err=%v", retiredInput, err)
	}
	if err := tx.QueryRowContext(ctx, `select input from dorf.job_messages where job_id='job-coding-retired'`).Scan(&retiredInput); err != nil || retiredInput != "edit source" {
		t.Fatalf("coding retirement changed retained input: input=%q err=%v", retiredInput, err)
	}
	var resourceID, retainedNonce string
	var providerID sql.NullString
	if err := tx.QueryRowContext(ctx, `select s.active_resource_id,r.ownership_nonce,r.provider_id
 from dorf.sandboxes s join dorf.sandbox_resources r on r.id=s.active_resource_id
 where s.id='sandbox-current'`).Scan(&resourceID, &retainedNonce, &providerID); err != nil || resourceID != "sandbox-current:initial" || retainedNonce != strings.Repeat("d", 64) || providerID.Valid {
		t.Fatalf("migrated resource=%q provider=%v ownership preserved=%t err=%v", resourceID, providerID, retainedNonce == strings.Repeat("d", 64), err)
	}
	var jobRevision, candidateRevision, artifact string
	var activeRevision sql.NullString
	if err := tx.QueryRowContext(ctx, `select j.sandbox_profile_revision,p.candidate_revision,p.active_revision,r.artifact
 from dorf.jobs j join dorf.sandbox_profiles p on p.name=j.sandbox_profile
 join dorf.sandbox_profile_revisions r on (r.name,r.definition_hash)=(j.sandbox_profile,j.sandbox_profile_revision)
 where j.id='job-current'`).Scan(&jobRevision, &candidateRevision, &activeRevision, &artifact); err != nil || jobRevision != strings.Repeat("b", 64) || candidateRevision != jobRevision || activeRevision.Valid || artifact != strings.Repeat("a", 64) {
		t.Fatalf("migrated binding=%q candidate=%q active=%v artifact=%q err=%v", jobRevision, candidateRevision, activeRevision, artifact, err)
	}
	var creatorID sql.NullString
	var reference string
	if err := tx.QueryRowContext(ctx, `select created_by_client_id,client_reference from dorf.jobs where id='job-current'`).Scan(&creatorID, &reference); err != nil || creatorID.Valid || reference != "" {
		t.Fatalf("legacy attribution was invented: creator=%v reference=%q err=%v", creatorID, reference, err)
	}
	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatalf("attribution migration replay: %v", err)
	}
	var retainedInput string
	if err := tx.QueryRowContext(ctx, `select input from dorf.job_messages where id='message-current'`).Scan(&retainedInput); err != nil || retainedInput != "run direct caller intent" {
		t.Fatalf("original Message changed during migration: %q err=%v", retainedInput, err)
	}
	var migrationCount int
	if err := tx.QueryRowContext(ctx, `select count(*) from dorf.schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != len(dorfMigrations) {
		t.Fatalf("migration count=%d", migrationCount)
	}
}
