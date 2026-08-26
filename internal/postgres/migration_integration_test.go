package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestCurrentBaselineIsImmutable(t *testing.T) {
	baseline, err := migrationFiles.ReadFile("migrations/001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(baseline)); got != "3db2b68dc915569b756fcde0493688425f4996ee1c35b361fa959e9afbc22c00" {
		t.Fatalf("current 001_baseline.sql checksum=%s", got)
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

func TestCurrentBaselineReplaysIdempotently(t *testing.T) {
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
	baseline, err := migrationFiles.ReadFile("migrations/001_baseline.sql")
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
	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatalf("baseline replay: %v", err)
	}
	var migrationCount int
	if err := tx.QueryRowContext(ctx, `select count(*) from dorf.schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration count=%d", migrationCount)
	}
}
