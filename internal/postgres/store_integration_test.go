package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresAdmissionSeparatesProductFactsAndSchedulesOnce(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close PostgreSQL connection: %v", err)
		}
	})
	ctx := context.Background()
	store := postgres.Store{DB: db}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	client, err := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	workflow.Register(client, spine.Service{Store: store})
	key := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	input := postgres.NewJob{AdmissionKey: key, Goal: "Complete goal", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fb", Branch: "dorf/integration", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high"}
	first, created, err := workflow.Admit(ctx, store, client, input)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first admission was not created")
	}
	taskIDs := []string{first.TaskID}
	t.Cleanup(func() {
		for _, taskID := range taskIDs {
			if _, err := db.ExecContext(context.Background(), `select absurd.cancel_task($1,$2::uuid)`, config.QueueName, taskID); err != nil {
				t.Errorf("cancel Absurd integration task %s: %v", taskID, err)
			}
		}
	})
	second, created, err := workflow.Admit(ctx, store, client, input)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("repeat admission created another Job")
	}
	if first.ID != second.ID || first.TaskID != second.TaskID {
		t.Fatalf("repeat admission diverged: %#v %#v", first, second)
	}
	if _, err := workflow.ScheduleCleanup(ctx, store, client, first.ID); err == nil {
		t.Fatal("cleanup was allowed to race an unobserved durable run")
	}
	var jobs, tasks int
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.jobs where admission_key=$1`, key).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.t_dorf_jobs where task_id=$1::uuid`, first.TaskID).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || tasks != 1 {
		t.Fatalf("jobs=%d tasks=%d, want one of each", jobs, tasks)
	}
	var transcriptColumns int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.columns where table_schema='dorf' and (column_name like '%transcript%' or column_name in ('messages','items','context'))`).Scan(&transcriptColumns); err != nil {
		t.Fatal(err)
	}
	if transcriptColumns != 0 {
		t.Fatalf("Dorf schema contains %d harness-owned transcript/context columns", transcriptColumns)
	}
	session, err := store.BeginAction(ctx, first.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "native-session-" + first.ID}); err != nil {
		t.Fatal(err)
	}
	turn, err := store.BeginAction(ctx, first.ID, spine.ActionTurnStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, turn.ID, spine.Receipt{ExternalID: "native-turn-" + first.ID, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveRun(ctx, first.ID, spine.Observation{SessionID: "native-session-" + first.ID, AgentRunID: turn.ID, TurnID: "native-turn-" + first.ID, Outcome: "completed"}); err != nil {
		t.Fatal(err)
	}
	scheduled, err := workflow.ScheduleCleanup(ctx, store, client, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskIDs = append(taskIDs, scheduled.CleanupTaskID)
	if err := store.CompleteCleanup(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	repeated, err := workflow.ScheduleCleanup(ctx, store, client, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.CleanupState != spine.CleanupComplete || repeated.CleanupTaskID != scheduled.CleanupTaskID {
		t.Fatalf("repeat cleanup regressed its terminal: first=%#v repeated=%#v", scheduled, repeated)
	}
}
