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

func TestMigrationsPreserveDirectSessionAndReplaySafely(t *testing.T) {
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
	if _, err := tx.ExecContext(ctx, `
update dorf.jobs set workflow_attention='needs observation',workflow_attention_source='operation:test',workflow_attention_at=clock_timestamp() where id='job-current';
update dorf.agent_runs set state='completed',harness='codex',thread_id='thread-current',
    turn_id='turn-current',turn_outcome='completed' where id='run-current';
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input)
values('message-queued','job-current','human','queued',2,'continue');
insert into dorf.agent_runs(id,job_id,message_id,role,state,sandbox_id)
values('run-queued','job-current','message-queued','direct','pending','sandbox-current')`); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []struct{ harness, thread string }{{"codex", "other-thread"}, {"pi", "thread-current"}} {
		if _, err := tx.ExecContext(ctx, `savepoint conflicting_thread`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `update dorf.agent_runs set harness=$1,thread_id=$2 where id='run-queued'`, binding.harness, binding.thread); err != nil {
			t.Fatal(err)
		}
		if err := migrateDorf(ctx, tx); err == nil || !strings.Contains(err.Error(), "conflicting or incomplete retained Thread bindings") {
			t.Fatalf("conflicting Thread migration: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `rollback to savepoint conflicting_thread`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(ctx, `savepoint profile_mismatch;
update dorf.agent_runs set harness='pi' where id='run-current'`); err != nil {
		t.Fatal(err)
	}
	if err := migrateDorf(ctx, tx); err == nil || !strings.Contains(err.Error(), "Thread Harness disagrees with its admitted profile") {
		t.Fatalf("profile mismatch migration: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `rollback to savepoint profile_mismatch`); err != nil {
		t.Fatal(err)
	}
	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatalf("baseline replay: %v", err)
	}
	var attention, source string
	var attentionRecorded bool
	if err := tx.QueryRowContext(ctx, `select execution_attention,execution_attention_source,execution_attention_at is not null from dorf.sessions where id='job-current'`).Scan(&attention, &source, &attentionRecorded); err != nil || attention != "needs observation" || source != "operation:test" || !attentionRecorded {
		t.Fatalf("migration lost execution attention: detail=%q source=%q recorded=%t err=%v", attention, source, attentionRecorded, err)
	}
	var resourceID, retainedNonce string
	var providerID sql.NullString
	if err := tx.QueryRowContext(ctx, `select s.active_resource_id,r.ownership_nonce,r.provider_id
 from dorf.sandboxes s join dorf.sandbox_resources r on r.id=s.active_resource_id
 where s.id='sandbox-current'`).Scan(&resourceID, &retainedNonce, &providerID); err != nil || resourceID != "sandbox-current:initial" || retainedNonce != strings.Repeat("d", 64) || providerID.Valid {
		t.Fatalf("migrated resource=%q provider=%v ownership preserved=%t err=%v", resourceID, providerID, retainedNonce == strings.Repeat("d", 64), err)
	}
	var sessionRevision, candidateRevision, artifact string
	var activeRevision sql.NullString
	if err := tx.QueryRowContext(ctx, `select j.sandbox_profile_revision,p.candidate_revision,p.active_revision,r.artifact
 from dorf.sessions j join dorf.sandbox_profiles p on p.name=j.sandbox_profile
 join dorf.sandbox_profile_revisions r on (r.name,r.definition_hash)=(j.sandbox_profile,j.sandbox_profile_revision)
 where j.id='job-current'`).Scan(&sessionRevision, &candidateRevision, &activeRevision, &artifact); err != nil || sessionRevision != strings.Repeat("b", 64) || candidateRevision != sessionRevision || activeRevision.Valid || artifact != strings.Repeat("a", 64) {
		t.Fatalf("migrated binding=%q candidate=%q active=%v artifact=%q err=%v", sessionRevision, candidateRevision, activeRevision, artifact, err)
	}
	var creatorID sql.NullString
	var reference string
	if err := tx.QueryRowContext(ctx, `select created_by_client_id,client_reference from dorf.sessions where id='job-current'`).Scan(&creatorID, &reference); err != nil || creatorID.Valid || reference != "" {
		t.Fatalf("legacy attribution was invented: creator=%v reference=%q err=%v", creatorID, reference, err)
	}
	if err := migrateDorf(ctx, tx); err != nil {
		t.Fatalf("attribution migration replay: %v", err)
	}
	var harness, thread string
	if err := tx.QueryRowContext(ctx, `select p.harness,j.thread_id from dorf.sessions j join dorf.sandbox_profile_revisions p on p.name=j.sandbox_profile and p.definition_hash=j.sandbox_profile_revision where j.id='job-current'`).Scan(&harness, &thread); err != nil || harness != "codex" || thread != "thread-current" {
		t.Fatalf("migrated Session Thread=%s/%s err=%v", harness, thread, err)
	}
	var queuedThread sql.NullString
	if err := tx.QueryRowContext(ctx, `select thread_id from dorf.agent_runs where id='run-queued'`).Scan(&queuedThread); err != nil || queuedThread.Valid {
		t.Fatalf("migration changed queued delivery attribution: thread=%v err=%v", queuedThread, err)
	}
	var retainedInput string
	if err := tx.QueryRowContext(ctx, `select input from dorf.session_messages where id='message-current'`).Scan(&retainedInput); err != nil || retainedInput != "run direct caller intent" {
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
