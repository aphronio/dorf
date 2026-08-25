package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPublishedBaselineIsImmutable(t *testing.T) {
	baseline, err := migrationFiles.ReadFile("migrations/001_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(baseline)); got != "9640be723c7c91ca62d08b2a21421ca945c67cb7d4d89ba403f17659b1b9ccd7" {
		t.Fatalf("published 001_baseline.sql checksum=%s", got)
	}
}

func TestPublishedBaselineMigratesRetainedSandboxCustody(t *testing.T) {
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

	if _, err := tx.ExecContext(ctx, `
insert into dorf.sandbox_profiles(name,provider,harness,artifact,incus_network,incus_disk_size)
values('migration-profile','incus','codex','artifact','incusbr0','40GiB');
insert into dorf.jobs(id,admission_key,workflow_name,workflow_revision,goal,sandbox_profile,provider_connection,model,reasoning_effort)
values('job-migration','migration-admission','coding-to-proposal','3','migrate retained custody','migration-profile','primary','gpt-5.6-sol','high');
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input)
values
  ('message-main','job-migration','human','migration:main',1,'implement'),
  ('message-review','job-migration','workflow','migration:review',2,'review');
insert into dorf.sandboxes(id,job_id,ownership_nonce)
values
  ('sandbox-main','job-migration',repeat('1',64)),
  ('sandbox-review','job-migration',repeat('2',64));
insert into dorf.agent_runs(id,job_id,message_id,role,state,sandbox_id)
values('agent-run-main','job-migration','message-main','implement','pending','sandbox-main');
insert into dorf.agent_runs(id,job_id,message_id,role,state,input_revision,capability,sandbox_id,submission_nonce)
values('agent-run-review','job-migration','message-review','general','pending','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','immutable-read-only','sandbox-review',repeat('3',64));
`); err != nil {
		t.Fatal(err)
	}

	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatal(err)
	}
	// Development deployments could have the current table shape while their
	// edited baseline still recorded only 001. Reapply 002 to that exact drift.
	if _, err := tx.ExecContext(ctx, `delete from dorf.schema_migrations where name='002_sandbox_custody.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatalf("migration from drifted baseline: %v", err)
	}
	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatalf("migration ledger replay: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
insert into dorf.jobs(id,admission_key,workflow_name,workflow_revision,goal,sandbox_profile,provider_connection,model,reasoning_effort)
values('job-client-migration','client-migration-admission','','','run direct caller intent','migration-profile','primary','gpt-5.6-sol','high');
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input)
values('message-client-migration','job-client-migration','human','dorf:initial',1,'run direct caller intent');
insert into dorf.sandboxes(id,job_id,name,ownership_nonce)
values('sandbox-client-migration','job-client-migration','default',repeat('4',64));
insert into dorf.agent_runs(id,job_id,message_id,role,state,sandbox_id)
values('agent-run-client-migration','job-client-migration','message-client-migration','direct','pending','sandbox-client-migration');
`); err != nil {
		t.Fatalf("client-directed Job after retained migration: %v", err)
	}

	rows, err := tx.QueryContext(ctx, `select id,name from dorf.sandboxes where job_id='job-migration' order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatal(err)
		}
		got[id] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["sandbox-main"] != "default" || got["sandbox-review"] != "agent-run-review" {
		t.Fatalf("migrated Sandbox names=%v", got)
	}

	reviewRun, err := dbsql.New(tx).GetReviewRun(ctx, "agent-run-review")
	if err != nil {
		t.Fatal(err)
	}
	if reviewRun.SandboxName != "agent-run-review" || reviewRun.SandboxID != "sandbox-review" {
		t.Fatalf("review projection=%#v", reviewRun)
	}
	if _, err := tx.ExecContext(ctx, `update dorf.jobs set cleanup_state='requested' where id='job-migration'`); err != nil {
		t.Fatalf("requested cleanup state: %v", err)
	}
	var artifacts, drafts bool
	if err := tx.QueryRowContext(ctx, `
select
  to_regclass('dorf.artifacts') is not null,
  to_regclass('dorf.codebase_investigation_drafts') is not null
`).Scan(&artifacts, &drafts); err != nil {
		t.Fatal(err)
	}
	if artifacts || drafts {
		t.Fatalf("artifacts=%t drafts=%t", artifacts, drafts)
	}
}
