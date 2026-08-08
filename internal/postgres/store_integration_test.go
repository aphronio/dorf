package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func testDatabase(t *testing.T) (*sql.DB, postgres.Store, *absurd.Client) {
	t.Helper()
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.Store{DB: db}
	if err := store.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	client, err := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	workflow.Register(client, spine.Service{Store: store}, store)
	t.Cleanup(func() {
		client.Close()
		db.Close()
	})
	return db, store, client
}

func TestPostgresMessageIdempotencyConcurrentFIFOAndLowestUnsettled(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("message-integration-%d", time.Now().UnixNano())
	input := postgres.NewJob{AdmissionKey: key, Goal: "initial input", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/integration", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
	job, created, err := workflow.Admit(ctx, store, client, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	repeatedJob, created, err := workflow.Admit(ctx, store, client, input)
	if err != nil || created || repeatedJob.ID != job.ID || repeatedJob.TaskID != job.TaskID {
		t.Fatalf("idempotent Job admission=%#v created=%v err=%v", repeatedJob, created, err)
	}
	changedJob := input
	changedJob.Goal = "changed complete input"
	if _, _, err := workflow.Admit(ctx, store, client, changedJob); err == nil {
		t.Fatal("changed complete Job input under the same admission key did not conflict")
	}
	taskIDs := []string{job.TaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_, _ = db.ExecContext(context.Background(), `select absurd.cancel_task($1,$2::uuid)`, config.QueueName, id)
		}
	})

	first, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "client-retry", Input: "same text"})
	if err != nil || !created || first.Sequence != 2 {
		t.Fatalf("first message=%#v created=%v err=%v", first, created, err)
	}
	repeated, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "client-retry", Input: "same text"})
	if err != nil || created || repeated != first {
		t.Fatalf("idempotent message=%#v created=%v err=%v", repeated, created, err)
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "client-retry", Input: "changed"}); err == nil {
		t.Fatal("changed input under the same caller ID did not conflict")
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "client-retry", Input: "same text "}); err == nil {
		t.Fatal("byte-distinct complete input under the same caller ID did not conflict")
	}
	distinct, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "client-distinct", Input: "same text"})
	if err != nil || !created || distinct.ID == first.ID || distinct.Sequence != 3 {
		t.Fatalf("distinct identical message=%#v created=%v err=%v", distinct, created, err)
	}

	const concurrent = 12
	sequences := make(chan int64, concurrent)
	errors := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := range concurrent {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			message, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: fmt.Sprintf("concurrent-%02d", i), Input: "same concurrent text"})
			if err == nil {
				sequences <- message.Sequence
			}
			errors <- err
		}(i)
	}
	wg.Wait()
	close(sequences)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var got []int
	for sequence := range sequences {
		got = append(got, int(sequence))
	}
	sort.Ints(got)
	for i, sequence := range got {
		if sequence != i+4 {
			t.Fatalf("concurrent FIFO positions=%v", got)
		}
	}

	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "native-session-" + job.ID}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.NextDelivery(ctx, job.ID, "native-session-"+job.ID)
	if err != nil || delivery.Message.Sequence != 1 {
		t.Fatalf("lowest delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, delivery.AgentRun.ID, "native-turn-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	next, err := store.NextDelivery(ctx, job.ID, "native-session-"+job.ID)
	if err != nil || next.Message.Sequence != 2 || next.AgentRun.ID == delivery.AgentRun.ID {
		t.Fatalf("next delivery=%#v err=%v", next, err)
	}
	var runCount int
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.agent_runs where job_id=$1 and message_id is not null`, job.ID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != concurrent+3 {
		t.Fatalf("per-Job AgentRuns=%d want=%d (one stable identity per admitted input)", runCount, concurrent+3)
	}
	if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "native-turn-"+job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, next.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, next.AgentRun.ID, "native-turn-2-"+job.ID, "running"); err != nil {
		t.Fatal(err)
	}
	blocker, err := store.NativeMutationDelivery(ctx, job.ID)
	if err != nil || blocker == nil || blocker.AgentRun.State != spine.AgentRunActive {
		t.Fatalf("active native mutation=%#v err=%v", blocker, err)
	}
	stillOpen, err := store.Job(ctx, job.ID)
	if err != nil || !stillOpen.AdmissionOpen {
		t.Fatalf("native mutation inspection changed admission: %#v err=%v", stillOpen, err)
	}
	if err := store.BindNativeTurn(ctx, next.AgentRun.ID, "native-turn-2-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	var transcriptColumns int
	if err := db.QueryRowContext(ctx, `select count(*) from information_schema.columns where table_schema='dorf' and (column_name like '%transcript%' or column_name in ('messages','items','context'))`).Scan(&transcriptColumns); err != nil {
		t.Fatal(err)
	}
	if transcriptColumns != 0 {
		t.Fatalf("Dorf schema contains %d harness-owned transcript/context columns", transcriptColumns)
	}

	fenceEntered := make(chan struct{})
	releaseFence := make(chan struct{})
	fenceDone := make(chan error, 1)
	go func() {
		fenceDone <- store.WithJobFence(ctx, job.ID, func() error {
			close(fenceEntered)
			<-releaseFence
			return nil
		})
	}()
	<-fenceEntered
	type cleanupResult struct {
		job spine.Job
		err error
	}
	cleanupDone := make(chan cleanupResult, 1)
	go func() {
		cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
		cleanupDone <- cleanupResult{job: cleaning, err: err}
	}()
	select {
	case result := <-cleanupDone:
		close(releaseFence)
		t.Fatalf("cleanup crossed the active native-mutation fence: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
	duringActive, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "during-active", Input: "accepted without the long fence"})
	if err != nil || !created || duringActive.Sequence != concurrent+4 {
		close(releaseFence)
		t.Fatalf("nonblocking active-turn admission=%#v created=%v err=%v", duringActive, created, err)
	}
	close(releaseFence)
	if err := <-fenceDone; err != nil {
		t.Fatal(err)
	}
	cleanup := <-cleanupDone
	if cleanup.err != nil {
		t.Fatal(cleanup.err)
	}
	cleaning := cleanup.job
	taskIDs = append(taskIDs, cleaning.CleanupTaskID)
	if cleaning.AdmissionOpen {
		t.Fatal("cleanup did not durably close admission")
	}
	if retry, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "client-retry", Input: "same text"}); err != nil || created || retry != first {
		t.Fatalf("closed admission did not preserve idempotent retry: %#v %v %v", retry, created, err)
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "after-cleanup", Input: "late"}); err == nil {
		t.Fatal("cleanup allowed a new message")
	}
}

func TestExplicitSteerTargetsAndAcknowledgesExactActiveTurn(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, sessionID := prepareTransportIntegrationJob(t, store, "explicit-steer")
	active, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || active == nil {
		t.Fatalf("initial delivery=%#v err=%v", active, err)
	}
	if err := store.PrepareAgentRun(ctx, active.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, active.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	activeTurnID := "turn-active-" + job.ID
	if err := store.BindNativeTurn(ctx, active.AgentRun.ID, activeTurnID, "running"); err != nil {
		t.Fatal(err)
	}

	steerInput := postgres.NewMessage{JobID: job.ID, CallerID: "operator-steer", Input: "correct the active work", Intent: spine.MessageSteer}
	steer, created, err := store.AdmitMessage(ctx, steerInput)
	if err != nil || !created || steer.Intent != spine.MessageSteer || steer.TargetTurnID != activeTurnID {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	repeated, created, err := store.AdmitMessage(ctx, steerInput)
	if err != nil || created || repeated != steer {
		t.Fatalf("idempotent steer=%#v created=%v err=%v", repeated, created, err)
	}
	changed := steerInput
	changed.Intent = spine.MessageFollow
	if _, _, err := store.AdmitMessage(ctx, changed); err == nil {
		t.Fatal("same caller identity accepted a changed delivery intent")
	}
	delivery, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || delivery == nil || delivery.Message.ID != steer.ID || delivery.AgentRun.SessionID != sessionID {
		t.Fatalf("steer delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, activeTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeSteer(ctx, delivery.AgentRun.ID, activeTurnID, "inProgress"); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil || len(messages) != 2 || !messages[1].Delivered || messages[1].NativeTurnID != activeTurnID || messages[1].Intent != spine.MessageSteer {
		t.Fatalf("steer messages=%#v err=%v", messages, err)
	}
	next, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || next == nil || next.Message.ID != active.Message.ID || next.AgentRun.NativeTurnID != activeTurnID {
		t.Fatalf("active follow after steer=%#v err=%v", next, err)
	}
	other, _ := prepareTransportIntegrationJob(t, store, "steer-without-active-turn")
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: other.ID, CallerID: "invalid-steer", Input: "cannot target", Intent: spine.MessageSteer}); err == nil || !strings.Contains(err.Error(), "exact active regular native turn") {
		t.Fatalf("steer without active turn error=%v", err)
	}
}

func TestSharedSteersPersistEveryTerminalTargetOutcome(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			job, sessionID := prepareTransportIntegrationJob(t, store, "shared-steer-outcome-"+status)
			target, err := store.NextDelivery(ctx, job.ID, sessionID)
			if err != nil || target == nil {
				t.Fatalf("target delivery=%#v err=%v", target, err)
			}
			if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginTurnSubmission(ctx, target.AgentRun.ID); err != nil {
				t.Fatal(err)
			}
			targetTurnID := "turn-shared-" + job.ID
			if err := store.BindNativeTurn(ctx, target.AgentRun.ID, targetTurnID, "running"); err != nil {
				t.Fatal(err)
			}
			first, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "first-shared-steer", Input: "first accepted shared input", Intent: spine.MessageSteer})
			if err != nil || !created {
				t.Fatalf("first steer=%#v created=%v err=%v", first, created, err)
			}
			second, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "second-shared-steer", Input: "second accepted shared input", Intent: spine.MessageSteer})
			if err != nil || !created {
				t.Fatalf("second steer=%#v created=%v err=%v", second, created, err)
			}
			firstDelivery, err := store.NextDelivery(ctx, job.ID, sessionID)
			if err != nil || firstDelivery == nil || firstDelivery.Message.ID != first.ID {
				t.Fatalf("first steer delivery=%#v err=%v", firstDelivery, err)
			}
			if err := store.PrepareAgentRun(ctx, firstDelivery.AgentRun.ID, targetTurnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginTurnSubmission(ctx, firstDelivery.AgentRun.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindNativeSteer(ctx, firstDelivery.AgentRun.ID, targetTurnID, "inProgress"); err != nil {
				t.Fatal(err)
			}
			secondDelivery, err := store.NextDelivery(ctx, job.ID, sessionID)
			if err != nil || secondDelivery == nil || secondDelivery.Message.ID != second.ID {
				t.Fatalf("second steer delivery=%#v err=%v", secondDelivery, err)
			}
			if err := store.PrepareAgentRun(ctx, secondDelivery.AgentRun.ID, targetTurnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginTurnSubmission(ctx, secondDelivery.AgentRun.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindNativeTurn(ctx, target.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindNativeSteer(ctx, secondDelivery.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindNativeTurn(ctx, target.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindNativeSteer(ctx, firstDelivery.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			messages, err := store.Messages(ctx, job.ID)
			if err != nil || len(messages) != 3 {
				t.Fatalf("messages=%#v err=%v", messages, err)
			}
			for index, message := range messages[1:] {
				if message.Intent != spine.MessageSteer || message.TargetTurnID != targetTurnID || message.NativeTurnID != targetTurnID || message.NativeOutcome != status || message.State != spine.AgentRunCompleted {
					t.Fatalf("shared steer %d=%#v", index+1, message)
				}
			}
		})
	}
}

func TestSteerTerminalFallbackPreservesRequestAndSerializesLaterFIFO(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, sessionID := prepareTransportIntegrationJob(t, store, "steer-terminal-fallback")
	target, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, target.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + job.ID
	if err := store.BindNativeTurn(ctx, target.AgentRun.ID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "terminal-race-steer", Input: "preserve exact durable input", Intent: spine.MessageSteer})
	if err != nil || !created || steer.TargetTurnID != targetTurnID {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	if err := store.BindNativeTurn(ctx, target.AgentRun.ID, targetTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	fallback, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || fallback == nil || fallback.Message.ID != steer.ID || fallback.AgentRun.ID != spine.AgentRunID(steer.ID) {
		t.Fatalf("fallback delivery=%#v err=%v", fallback, err)
	}
	if err := store.PrepareAgentRun(ctx, fallback.AgentRun.ID, targetTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, fallback.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	actualTurnID := "turn-fallback-" + job.ID
	if err := store.BindNativeTurn(ctx, fallback.AgentRun.ID, actualTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	later, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "later-follow", Input: "later FIFO delivery"})
	if err != nil || !created {
		t.Fatalf("later=%#v created=%v err=%v", later, created, err)
	}
	active, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || active == nil || active.Message.ID != steer.ID || active.AgentRun.NativeTurnID != actualTurnID {
		t.Fatalf("active fallback selection=%#v err=%v", active, err)
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil || len(messages) != 3 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	if messages[1].Intent != spine.MessageSteer || messages[1].TargetTurnID != targetTurnID || messages[1].NativeTurnID != actualTurnID || messages[2].BlockingSeq != steer.Sequence {
		t.Fatalf("fallback projection=%#v later=%#v", messages[1], messages[2])
	}
	if err := store.BindNativeTurn(ctx, fallback.AgentRun.ID, actualTurnID, "failed"); err != nil {
		t.Fatal(err)
	}
	next, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || next == nil || next.Message.ID != later.ID || next.AgentRun.SessionID != sessionID {
		t.Fatalf("later delivery=%#v err=%v", next, err)
	}
	messages, err = store.Messages(ctx, job.ID)
	if err != nil || messages[1].State != spine.AgentRunFailed || messages[1].NativeOutcome != "failed" || messages[1].TargetTurnID != targetTurnID {
		t.Fatalf("terminal fallback evidence=%#v err=%v", messages[1], err)
	}
}

func TestAcceptedTerminalTurnAllowsSameSessionFollowFIFO(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			job, sessionID := prepareTransportIntegrationJob(t, store, "terminal-follow-"+status)
			first, err := store.NextDelivery(ctx, job.ID, sessionID)
			if err != nil || first == nil {
				t.Fatalf("first=%#v err=%v", first, err)
			}
			if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, ""); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginTurnSubmission(ctx, first.AgentRun.ID); err != nil {
				t.Fatal(err)
			}
			turnID := "turn-first-" + job.ID
			if err := store.BindNativeTurn(ctx, first.AgentRun.ID, turnID, "running"); err != nil {
				t.Fatal(err)
			}
			follow, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "queued-follow", Input: "continue after the accepted outcome"})
			if err != nil || !created || follow.Intent != spine.MessageFollow {
				t.Fatalf("follow=%#v created=%v err=%v", follow, created, err)
			}
			stillActive, err := store.NextDelivery(ctx, job.ID, sessionID)
			if err != nil || stillActive == nil || stillActive.Message.ID != first.Message.ID {
				t.Fatalf("FIFO crossed active turn: delivery=%#v err=%v", stillActive, err)
			}
			if err := store.BindNativeTurn(ctx, first.AgentRun.ID, turnID, status); err != nil {
				t.Fatal(err)
			}
			next, err := store.NextDelivery(ctx, job.ID, sessionID)
			if err != nil || next == nil || next.Message.ID != follow.ID || next.AgentRun.SessionID != sessionID {
				t.Fatalf("follow after %s=%#v err=%v", status, next, err)
			}
			if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, turnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BeginTurnSubmission(ctx, next.AgentRun.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindNativeTurn(ctx, next.AgentRun.ID, "turn-follow-"+job.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			messages, err := store.Messages(ctx, job.ID)
			if err != nil || len(messages) != 2 || messages[0].NativeOutcome != status || !messages[0].Delivered || messages[1].State != spine.AgentRunCompleted {
				t.Fatalf("preserved %s then follow=%#v err=%v", status, messages, err)
			}
		})
	}
}

func TestFailedAcceptedTurnRequiresLaterSuccessfulFollowBeforeCommit(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, sessionID := prepareTransportIntegrationJob(t, store, "failed-commit-gate")
	first, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || first == nil {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, first.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	firstTurnID := "turn-failed-" + job.ID
	if err := store.BindNativeTurn(ctx, first.AgentRun.ID, firstTurnID, "failed"); err != nil {
		t.Fatal(err)
	}
	if action, started, err := store.BeginCommit(ctx, job.ID, job.Revision); err != nil || started || action.ID != "" {
		t.Fatalf("failed accepted turn crossed commit gate: action=%#v started=%v err=%v", action, started, err)
	}
	follow, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "successful-follow", Input: "finish the coding workflow"})
	if err != nil || !created {
		t.Fatalf("follow=%#v created=%v err=%v", follow, created, err)
	}
	completeNextIntegrationRun(t, store, job.ID, sessionID, "turn-success-"+job.ID)
	if action, started, err := store.BeginCommit(ctx, job.ID, job.Revision); err != nil || !started || action.ID == "" {
		t.Fatalf("successful later follow did not open commit gate: action=%#v started=%v err=%v", action, started, err)
	}
}

func prepareTransportIntegrationJob(t *testing.T, store postgres.Store, label string) (spine.Job, string) {
	t.Helper()
	ctx := context.Background()
	revision := strings.Repeat("a", 40)
	key := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: key, Goal: "transport proof", Repository: "https://github.com/aphronio/dorf.git", Revision: revision, Branch: "dorf/" + label, ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	setup, err := store.BeginSetup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSetup(ctx, setup.ID, integrationEvidence(setup.ID, "repository-setup", setup.ID, "", revision, "7"), spine.CommandObservation{Command: "prepare", StartedAt: now, FinishedAt: now}, []spine.DeclaredCheck{{Name: "check", Command: "go test ./focused"}}); err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "session-" + job.ID
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: sessionID}); err != nil {
		t.Fatal(err)
	}
	return job, sessionID
}

func TestRevisionChecksEvidenceRepairAndCleanupRetention(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("revision-evidence-%d", time.Now().UnixNano())
	start := strings.Repeat("1", 40)
	first := strings.Repeat("2", 40)
	second := strings.Repeat("3", 40)
	input := postgres.NewJob{AdmissionKey: key, Goal: "bounded implementation", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/revision-evidence", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
	job, created, err := workflow.Admit(ctx, store, client, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Round(time.Microsecond)
	setup, err := store.BeginSetup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	setupEvidence := integrationEvidence(setup.ID, "repository-setup", setup.ID, "", start, "a")
	if err := store.RecordSetup(ctx, setup.ID, setupEvidence, spine.CommandObservation{Command: "prepare", StartedAt: now, FinishedAt: now}, []spine.DeclaredCheck{{Name: "check", Command: "go test ./..."}}); err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "session-" + job.ID}); err != nil {
		t.Fatal(err)
	}
	completeNextIntegrationRun(t, store, job.ID, "session-"+job.ID, "turn-implementation-"+job.ID)
	commit, started, err := store.BeginCommit(ctx, job.ID, start)
	if err != nil || !started {
		t.Fatalf("first commit started=%v err=%v", started, err)
	}
	firstRevisionEvidence := integrationEvidence(commit.ID, "git-revision", commit.ID, "", first, "b")
	if err := store.RecordRevision(ctx, commit.ID, spine.CommitObservation{Parent: start, Revision: first, Tree: strings.Repeat("4", 40), Branch: input.Branch}, firstRevisionEvidence); err != nil {
		t.Fatal(err)
	}
	failed, err := store.BeginCheck(ctx, job.ID, first, "check", "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	failedEvidence := integrationEvidence(failed.ID, "check-output", "", failed.ID, first, "c")
	if err := store.RecordCheck(ctx, failed, failedEvidence, spine.CommandObservation{Command: failed.Command, ExitCode: 1, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	failed.State, failed.ExitCode, failed.EvidenceDigest = "failed", 1, failedEvidence.Digest
	repair, created, err := store.AdmitRepair(ctx, failed)
	if err != nil || !created || repair.Sequence != 2 {
		t.Fatalf("repair=%#v created=%v err=%v", repair, created, err)
	}
	repeated, created, err := store.AdmitRepair(ctx, failed)
	if err != nil || created || repeated != repair {
		t.Fatalf("repeated repair=%#v created=%v err=%v", repeated, created, err)
	}
	completeNextIntegrationRun(t, store, job.ID, "session-"+job.ID, "turn-repair-"+job.ID)
	job, err = store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondCommit, started, err := store.BeginCommit(ctx, job.ID, job.Revision)
	if err != nil || !started {
		t.Fatalf("second commit started=%v err=%v", started, err)
	}
	secondRevisionEvidence := integrationEvidence(secondCommit.ID, "git-revision", secondCommit.ID, "", second, "d")
	if err := store.RecordRevision(ctx, secondCommit.ID, spine.CommitObservation{Parent: first, Revision: second, Tree: strings.Repeat("5", 40), Branch: input.Branch}, secondRevisionEvidence); err != nil {
		t.Fatal(err)
	}
	passing, err := store.BeginCheck(ctx, job.ID, second, "check", "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	passingEvidence := integrationEvidence(passing.ID, "check-output", "", passing.ID, second, "e")
	if err := store.RecordCheck(ctx, passing, passingEvidence, spine.CommandObservation{Command: passing.Command, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, job.ID, second, nil); err == nil || !strings.Contains(err.Error(), "verified Evidence") {
		t.Fatalf("row-only readiness error=%v", err)
	}
	if err := store.MarkReady(ctx, job.ID, second, []string{passingEvidence.ID}); err != nil {
		t.Fatal(err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.WorkflowPhase != "ready" || job.Revision != second || job.StartingRevision != start || job.RepairCount != 1 {
		t.Fatalf("ready job=%#v err=%v", job, err)
	}
	checks, err := store.Checks(ctx, job.ID)
	byRevision := map[string]spine.Check{}
	for _, check := range checks {
		byRevision[check.Revision] = check
	}
	if err != nil || len(checks) != 2 || byRevision[first].State != "failed" || byRevision[second].State != "passed" {
		t.Fatalf("historical/current Checks=%#v err=%v", checks, err)
	}
	records, err := store.Evidence(ctx, job.ID)
	if err != nil || len(records) != 5 {
		t.Fatalf("Evidence=%#v err=%v", records, err)
	}
	sandboxCreate, err := store.BeginAction(ctx, job.ID, spine.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, sandboxCreate.ID, spine.Receipt{ExternalID: spine.MainSandboxName(job.ID)}); err != nil {
		t.Fatal(err)
	}
	routeCreate, err := store.BeginAction(ctx, job.ID, spine.ActionRouteCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, routeCreate.ID, spine.Receipt{ExternalID: spine.ProviderRouteID(routeCreate.ID)}); err != nil {
		t.Fatal(err)
	}
	routeDelete, err := store.BeginAction(ctx, job.ID, spine.ActionRouteRevoke)
	if err != nil {
		t.Fatal(err)
	}
	cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, cleaning.CleanupTaskID) })
	if err := store.CompleteAction(ctx, routeDelete.ID, spine.Receipt{ExternalID: spine.ProviderRouteID(routeCreate.ID), Outcome: "revoked"}); err != nil {
		t.Fatal(err)
	}
	sandboxDelete, err := store.BeginAction(ctx, job.ID, spine.ActionSandboxDelete)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, sandboxDelete.ID, spine.Receipt{ExternalID: spine.MainSandboxName(job.ID), Outcome: "deleted"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	retainedChecks, _ := store.Checks(ctx, job.ID)
	retainedEvidence, _ := store.Evidence(ctx, job.ID)
	if len(retainedChecks) != len(checks) || len(retainedEvidence) != len(records) {
		t.Fatalf("cleanup lost audit facts Checks=%d Evidence=%d", len(retainedChecks), len(retainedEvidence))
	}
}

func TestAtomicReviewPolicyPersistsNoReviewAndStableSelectedRuns(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	for _, test := range []struct {
		name      string
		paths     []string
		requested []policy.Role
		roles     []policy.Role
		phase     string
	}{
		{name: "explicit no review", paths: []string{"docs/review.md"}, phase: "ready"},
		{name: "mandatory selected role", paths: []string{"internal/auth/session.go"}, roles: []policy.Role{policy.RoleAuthAuthority}, phase: "reviewing"},
		{name: "implementation request is bound before policy", paths: []string{"internal/auth/session.go"}, requested: []policy.Role{policy.RoleBrowserUI, policy.RoleCriticalBoundary, policy.RolePerformance}, roles: []policy.Role{policy.RoleAuthAuthority, policy.RoleBrowserUI, policy.RoleCriticalBoundary, policy.RolePerformance}, phase: "reviewing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, revision, verifiedID := prepareReviewIntegrationJob(t, store, test.name)
			if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
				t.Fatal(err)
			}
			waiting, err := store.Job(ctx, job.ID)
			if err != nil || waiting.WorkflowPhase != "review-activation" {
				t.Fatalf("post-Checks activation boundary Job=%#v err=%v", waiting, err)
			}
			record, created, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: test.requested})
			if err != nil || !created || record.State != "pending" || !slices.Equal(record.RequestedRoles, test.requested) {
				t.Fatalf("pending activation=%#v created=%v err=%v", record, created, err)
			}
			retried, created, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: test.requested})
			if err != nil || created || !slices.Equal(retried.RequestedRoles, test.requested) {
				t.Fatalf("idempotent activation=%#v created=%v err=%v", retried, created, err)
			}
			facts, err := policy.FactsFromPaths(job.StartingRevision, revision, test.paths, true, false)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := policy.ReviewPolicy(facts, test.requested)
			if err != nil {
				t.Fatal(err)
			}
			record.Facts, record.Initial, record.Final = facts, plan, plan
			if err := store.RecordReviewPolicy(ctx, record); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordReviewPolicy(ctx, record); err != nil {
				t.Fatalf("idempotent policy retry: %v", err)
			}
			updated, err := store.Job(ctx, job.ID)
			if err != nil || updated.WorkflowPhase != test.phase {
				t.Fatalf("updated Job=%#v err=%v", updated, err)
			}
			persisted, err := store.ReviewPlan(ctx, job.ID, revision)
			if err != nil || persisted.State != "final" || persisted.PolicyDigest == "" || persisted.Final.Decision != plan.Decision {
				t.Fatalf("persisted plan=%#v err=%v", persisted, err)
			}
			runs, err := store.ReviewRuns(ctx, job.ID, revision)
			if len(test.roles) == 0 {
				if err != nil || len(runs) != 0 || persisted.Final.Decision != "no-review" {
					t.Fatalf("no-review runs=%#v plan=%#v err=%v", runs, persisted, err)
				}
			} else {
				if err != nil || len(runs) != len(test.roles) {
					t.Fatalf("selected runs=%#v err=%v", runs, err)
				}
				for i, role := range test.roles {
					if runs[i].Role != string(role) || runs[i].ID != spine.ReviewAgentRunID(job.ID, revision, string(role)) || runs[i].Capability != spine.ReviewReadOnlyCapability || runs[i].Revision != revision || runs[i].ReviewerSandboxID != spine.ReviewSandboxName(runs[i].ID) || len(runs[i].ReviewerOwnerNonce) != 64 || len(runs[i].SubmissionNonce) != 64 || len(runs[i].InputDigest) != 64 || runs[i].ReviewerSandboxState != "pending" || runs[i].ReviewerRouteState != "pending" || runs[i].CheckoutState != "pending" {
						t.Fatalf("selected runs=%#v err=%v", runs, err)
					}
					for prior := 0; prior < i; prior++ {
						if runs[prior].ReviewerSandboxID == runs[i].ReviewerSandboxID || runs[prior].ReviewerOwnerNonce == runs[i].ReviewerOwnerNonce || runs[prior].SubmissionNonce == runs[i].SubmissionNonce {
							t.Fatalf("review Roles share an isolated resource identity: %#v", runs)
						}
					}
				}
			}
			changed := record
			changed.Initial.Decision = "invalid-retry-change"
			if err := store.RecordReviewPolicy(ctx, changed); err == nil || !strings.Contains(err.Error(), "changed across retry") {
				t.Fatalf("changed atomic policy error=%v", err)
			}
		})
	}
}

func TestRejectedMaterialReviewClaimRemainsDurableAndReachesReady(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, verifiedID := prepareReviewIntegrationJob(t, store, "rejected-material-ready")
	if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
		t.Fatal(err)
	}
	record, created, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: []policy.Role{policy.RoleCriticalBoundary}})
	if err != nil || !created {
		t.Fatalf("activation=%#v created=%t err=%v", record, created, err)
	}
	facts, err := policy.FactsFromPaths(job.StartingRevision, revision, []string{"docs/review.md"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts, record.RequestedRoles)
	if err != nil {
		t.Fatal(err)
	}
	record.Facts, record.Initial, record.Final = facts, plan, plan
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || len(runs) != 1 {
		t.Fatalf("review runs=%#v err=%v", runs, err)
	}
	run := runs[0].AgentRun
	prepareReviewBoundaryIntegration(t, store, run)
	claim := integrationEvidence(run.ID, "review-finding", run.ActionID, "", revision, "6")
	observed := integrationEvidence(run.ID, "review-native-observation", run.ActionID, "", revision, "7")
	finding := spine.ReviewFinding{RunID: run.ID, Revision: revision, Role: policy.RoleCriticalBoundary, Material: true, Summary: "claim", Rationale: "requires adjudication", AffectedRoles: []policy.Role{policy.RoleCriticalBoundary}, AffectedChecks: []string{"check"}}
	if err := store.RecordReviewResult(ctx, run.ID, spine.NativeTurn{ID: "turn-" + run.ID, Status: "completed"}, claim, observed, finding); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.AdmitReviewRepair(ctx, job.ID, run.ID); err != nil || !created {
		t.Fatalf("repair created=%t err=%v", created, err)
	}
	completeNextIntegrationRun(t, store, job.ID, "session-"+job.ID, "turn-review-repair-"+job.ID)
	if err := store.RejectReviewFinding(ctx, job.ID, revision); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReviewReady(ctx, job.ID, revision); err != nil {
		t.Fatal(err)
	}
	ready, err := store.Job(ctx, job.ID)
	runs, runsErr := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || runsErr != nil || ready.WorkflowPhase != "ready" || len(runs) != 1 || runs[0].Finding == nil || !runs[0].Finding.Material || runs[0].Finding.Adjudication != "rejected" || runs[0].Finding.EvidenceID != claim.ID {
		t.Fatalf("ready=%#v runs=%#v err=%v runsErr=%v", ready, runs, err, runsErr)
	}
}

func TestReviewResourceReceiptsAndCleanupAreExactAndRetrySafe(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, verifiedID := prepareReviewIntegrationJob(t, store, "review-resource-cleanup")
	if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: []policy.Role{policy.RoleCriticalBoundary}})
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := policy.FactsFromPaths(job.StartingRevision, revision, []string{"docs/review.md"}, true, false)
	plan, _ := policy.ReviewPolicy(facts, record.RequestedRoles)
	record.Facts, record.Initial, record.Final = facts, plan, plan
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	run := runs[0].AgentRun
	sandbox, err := store.BeginReviewSandbox(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, sandbox.ID, spine.Receipt{ExternalID: "foreign-review-sandbox"}); err == nil {
		t.Fatal("mismatched reviewer Sandbox receipt was accepted")
	}
	prepareReviewBoundaryIntegration(t, store, run)
	routeDelete, err := store.BeginReviewRouteCleanup(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, routeDelete.ID, spine.Receipt{ExternalID: "route-" + run.ID, Outcome: "revoked"}); err != nil {
		t.Fatal(err)
	}
	sandboxDelete, err := store.BeginReviewSandboxCleanup(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, sandboxDelete.ID, spine.Receipt{ExternalID: run.ReviewerSandboxID, Outcome: "deleted"}); err != nil {
		t.Fatal(err)
	}
	for _, begin := range []func(context.Context, string) (spine.Action, error){store.BeginReviewRouteCleanup, store.BeginReviewSandboxCleanup} {
		action, err := begin(ctx, run.ID)
		if err != nil || action.State != spine.ActionSucceeded {
			t.Fatalf("cleanup retry action=%#v err=%v", action, err)
		}
	}
	settled, err := store.ReviewRun(ctx, run.ID)
	if err != nil || settled.ReviewerRouteState != "revoked" || settled.ReviewerSandboxState != "deleted" || settled.PostReviewState != "verified" {
		t.Fatalf("settled review resource=%#v err=%v", settled, err)
	}
}

func TestMainCleanupReceiptsAreExactAndCannotMutateAnotherJob(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	newJob := func(label string) spine.Job {
		input := postgres.NewJob{AdmissionKey: fmt.Sprintf("main-cleanup-fence-%s-%d", label, time.Now().UnixNano()), Goal: "fence exact cleanup " + label, Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40), Branch: "dorf/main-cleanup-" + label, ProviderConnection: "primary", ProviderGatewayState: "/tmp/gateway-" + label, Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
		job, created, err := store.Admit(ctx, input)
		if err != nil || !created {
			t.Fatalf("admit %s created=%t err=%v", label, created, err)
		}
		sandbox, _ := store.BeginAction(ctx, job.ID, spine.ActionSandboxCreate)
		if err := store.CompleteAction(ctx, sandbox.ID, spine.Receipt{ExternalID: spine.MainSandboxName(job.ID)}); err != nil {
			t.Fatal(err)
		}
		route, _ := store.BeginAction(ctx, job.ID, spine.ActionRouteCreate)
		if err := store.CompleteAction(ctx, route.ID, spine.Receipt{ExternalID: spine.ProviderRouteID(route.ID)}); err != nil {
			t.Fatal(err)
		}
		job, _ = store.Job(ctx, job.ID)
		return job
	}
	target, sentinel := newJob("target"), newJob("sentinel")
	routeDelete, _ := store.BeginAction(ctx, target.ID, spine.ActionRouteRevoke)
	if err := store.CompleteAction(ctx, routeDelete.ID, spine.Receipt{ExternalID: sentinel.RouteID, Outcome: "revoked"}); err == nil {
		t.Fatal("another Job route receipt was accepted")
	}
	targetAfter, _ := store.Job(ctx, target.ID)
	sentinelAfter, _ := store.Job(ctx, sentinel.ID)
	if targetAfter.RouteID != target.RouteID || sentinelAfter.RouteID != sentinel.RouteID {
		t.Fatalf("route isolation target=%#v sentinel=%#v", targetAfter, sentinelAfter)
	}
	if err := store.CompleteAction(ctx, routeDelete.ID, spine.Receipt{ExternalID: "absent", Outcome: "revoked"}); err != nil {
		t.Fatal(err)
	}
	sandboxDelete, _ := store.BeginAction(ctx, target.ID, spine.ActionSandboxDelete)
	if err := store.CompleteAction(ctx, sandboxDelete.ID, spine.Receipt{ExternalID: sentinel.SandboxID, Outcome: "deleted"}); err == nil {
		t.Fatal("another Job Sandbox receipt was accepted")
	}
	if err := store.CompleteAction(ctx, sandboxDelete.ID, spine.Receipt{ExternalID: target.SandboxID, Outcome: "deleted"}); err != nil {
		t.Fatal(err)
	}
	sentinelAfter, _ = store.Job(ctx, sentinel.ID)
	if sentinelAfter.RouteID != sentinel.RouteID || sentinelAfter.SandboxID != sentinel.SandboxID {
		t.Fatalf("sentinel resource identities changed: before=%#v after=%#v", sentinel, sentinelAfter)
	}
}

func TestMainCreateIntentsReserveExactCleanupIdentitiesBeforeReceipts(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	input := postgres.NewJob{AdmissionKey: fmt.Sprintf("main-create-intent-%d", time.Now().UnixNano()), Goal: "reserve exact cleanup identities", Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40), Branch: "dorf/main-create-intent", ProviderConnection: "primary", ProviderGatewayState: "/tmp/gateway-main-create-intent", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%t err=%v", created, err)
	}
	if _, err := store.BeginAction(ctx, job.ID, spine.ActionSandboxCreate); err != nil {
		t.Fatal(err)
	}
	routeCreate, err := store.BeginAction(ctx, job.ID, spine.ActionRouteCreate)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.SandboxID != spine.MainSandboxName(job.ID) || reserved.SandboxState != "pending" || reserved.RouteID != spine.ProviderRouteID(routeCreate.ID) || reserved.RouteState != "pending" {
		t.Fatalf("reserved Job resources=%#v", reserved)
	}

	routeDelete, err := store.BeginAction(ctx, job.ID, spine.ActionRouteRevoke)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, routeDelete.ID, spine.Receipt{ExternalID: reserved.RouteID, Outcome: "revoked"}); err != nil {
		t.Fatal(err)
	}
	sandboxDelete, err := store.BeginAction(ctx, job.ID, spine.ActionSandboxDelete)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, sandboxDelete.ID, spine.Receipt{ExternalID: reserved.SandboxID, Outcome: "deleted"}); err != nil {
		t.Fatal(err)
	}
	settled, err := store.Job(ctx, job.ID)
	if err != nil || settled.RouteState != "revoked" || settled.SandboxState != "deleted" {
		t.Fatalf("settled Job resources=%#v err=%v", settled, err)
	}
}

func TestCleanupReviewEnumerationUsesPersistedResourcesAndRetainsHistoricalEvidence(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, verifiedID := prepareReviewIntegrationJob(t, store, "cleanup-review-resource-authority")
	if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: []policy.Role{policy.RoleCriticalBoundary}})
	if err != nil {
		t.Fatal(err)
	}
	facts, err := policy.FactsFromPaths(job.StartingRevision, revision, []string{"docs/review.md"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts, record.RequestedRoles)
	if err != nil {
		t.Fatal(err)
	}
	record.Facts, record.Initial, record.Final = facts, plan, plan
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	recorded, err := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || len(recorded) != 1 {
		t.Fatalf("recorded review runs=%#v err=%v", recorded, err)
	}

	historicalRevision := strings.Repeat("e", 40)
	historicalRunID := spine.ReviewAgentRunID(job.ID, historicalRevision, string(policy.RoleBrowserUI))
	historicalActionID := spine.ScopedActionID(job.ID, spine.ActionTurnStart, historicalRunID)
	if _, err := db.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state,scope_key,external_id) values($1,$2,'codex-turn-start','succeeded',$3,$4)`, historicalActionID, job.ID, historicalRunID, "turn-"+historicalRunID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := db.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,action_id,state,role,revision,capability,workspace,input_contract,output_contract,native_turn_id,native_outcome,started_at,finished_at) values($1,$2,$3,'completed',$4,$5,$6,'/historical/review','historical input','historical output',$7,'completed',$8,$8)`, historicalRunID, job.ID, historicalActionID, policy.RoleBrowserUI, historicalRevision, spine.ReviewReadOnlyCapability, "turn-"+historicalRunID, now); err != nil {
		t.Fatal(err)
	}
	historicalEvidence := integrationEvidence(historicalRunID, "review-finding", historicalActionID, "", historicalRevision, "f")
	historicalEvidence.Provenance = "claim"
	if _, err := db.ExecContext(ctx, `insert into dorf.evidence(id,job_id,digest,byte_size,media_type,producer,provenance,kind,action_id,revision,started_at,finished_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, historicalEvidence.ID, job.ID, historicalEvidence.Digest, historicalEvidence.ByteSize, historicalEvidence.MediaType, historicalEvidence.Producer, historicalEvidence.Provenance, historicalEvidence.Kind, historicalEvidence.ActionID, historicalEvidence.Revision, historicalEvidence.StartedAt, historicalEvidence.FinishedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.agent_runs set claim_evidence_id=$2 where id=$1`, historicalRunID, historicalEvidence.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.review_findings(run_id,job_id,revision,role,material,summary,rationale,affected_roles,affected_checks,evidence_id,adjudication,stale) values($1,$2,$3,$4,true,'historical claim','retained rejected finding','["browser-ui"]'::jsonb,'[]'::jsonb,$5,'rejected',true)`, historicalRunID, job.ID, historicalRevision, policy.RoleBrowserUI, historicalEvidence.ID); err != nil {
		t.Fatal(err)
	}

	allRuns, err := store.AllReviewRuns(ctx, job.ID)
	historicalIndex := slices.IndexFunc(allRuns, func(run spine.ReviewRunView) bool { return run.ID == historicalRunID })
	if err != nil || len(allRuns) != 2 || historicalIndex < 0 {
		t.Fatalf("aggregate review runs=%#v err=%v", allRuns, err)
	}
	historicalView := allRuns[historicalIndex]
	if !historicalView.Stale || historicalView.Finding == nil || historicalView.Finding.EvidenceID != historicalEvidence.ID || historicalView.Finding.Adjudication != "rejected" || !historicalView.Finding.Stale {
		t.Fatalf("historical aggregate hydration=%#v", historicalView)
	}

	cleanupRuns, err := store.CleanupReviewRuns(ctx, job.ID)
	if err != nil || len(cleanupRuns) != 1 || cleanupRuns[0].ID != recorded[0].ID || cleanupRuns[0].ReviewerSandboxID == "" || cleanupRuns[0].ReviewerSandboxState != "pending" || cleanupRuns[0].ReviewerRouteState != "pending" {
		t.Fatalf("cleanup resource authority runs=%#v err=%v", cleanupRuns, err)
	}
	historicalRuns, err := store.ReviewRuns(ctx, job.ID, historicalRevision)
	if err != nil || len(historicalRuns) != 1 || historicalRuns[0].ID != historicalRunID || historicalRuns[0].ReviewerSandboxID != "" || historicalRuns[0].ClaimEvidenceID != historicalEvidence.ID {
		t.Fatalf("historical inspection runs=%#v err=%v", historicalRuns, err)
	}
	evidence, err := store.Evidence(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evidence, func(record spine.Evidence) bool {
		return record.ID == historicalEvidence.ID && record.Revision == historicalRevision && record.Provenance == "claim"
	}) {
		t.Fatalf("historical review Evidence was not retained: %#v", evidence)
	}
}

func TestReviewSubmissionUncertaintyIsAtomicAndExactBindingCanRecover(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, verifiedID := prepareReviewIntegrationJob(t, store, "review-submission-uncertainty")
	if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: []policy.Role{policy.RoleCriticalBoundary}})
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := policy.FactsFromPaths(job.StartingRevision, revision, []string{"docs/review.md"}, true, false)
	plan, _ := policy.ReviewPolicy(facts, record.RequestedRoles)
	record.Facts, record.Initial, record.Final = facts, plan, plan
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	run := runs[0].AgentRun
	prepareReviewResourcesIntegration(t, store, run)
	if err := store.PrepareAgentRun(ctx, run.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginReviewSession(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UncertainReviewSubmission(ctx, run.ID, session.ID, "turn/start response lost"); err != nil {
		t.Fatal(err)
	}

	uncertain, err := store.ReviewRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sessionState, sessionOutcome, turnState, turnOutcome string
	if err := db.QueryRowContext(ctx, `select state,coalesce(external_outcome,'') from dorf.actions where id=$1`, session.ID).Scan(&sessionState, &sessionOutcome); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `select state,coalesce(external_outcome,'') from dorf.actions where id=$1`, run.ActionID).Scan(&turnState, &turnOutcome); err != nil {
		t.Fatal(err)
	}
	wantOutcome := spine.ReviewSubmissionUncertainOutcome + ": turn/start response lost"
	if uncertain.State != spine.AgentRunUncertain || uncertain.SessionID != "" || uncertain.NativeTurnID != "" || sessionState != "uncertain" || turnState != "uncertain" || sessionOutcome != wantOutcome || turnOutcome != wantOutcome {
		t.Fatalf("uncertain run=%#v session=%s/%q turn=%s/%q", uncertain, sessionState, sessionOutcome, turnState, turnOutcome)
	}

	controllerID := spine.ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "session-" + run.ID, Outcome: controllerID}); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, run.ID, "turn-"+run.ID, "running"); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.ReviewRun(ctx, run.ID)
	if err != nil || recovered.State != spine.AgentRunActive || recovered.SessionID != "session-"+run.ID || recovered.NativeTurnID != "turn-"+run.ID || recovered.ReviewerAppServer != controllerID {
		t.Fatalf("recovered run=%#v err=%v", recovered, err)
	}
}

func TestIsolatedUncertainReviewRunCanBeInterruptedForExactCleanup(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, verifiedID := prepareReviewIntegrationJob(t, store, "review-interrupt-cleanup")
	if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
		t.Fatal(err)
	}
	record, _, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: []policy.Role{policy.RoleCriticalBoundary}})
	if err != nil {
		t.Fatal(err)
	}
	facts, _ := policy.FactsFromPaths(job.StartingRevision, revision, []string{"docs/review.md"}, true, false)
	plan, _ := policy.ReviewPolicy(facts, record.RequestedRoles)
	record.Facts, record.Initial, record.Final = facts, plan, plan
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	runID := runs[0].ID
	if err := store.UncertainAgentRun(ctx, runID, "strict identity mismatch"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.InterruptReviewRun(ctx, runID, "admission closed; exact isolated reviewer resources are being reclaimed"); err != nil {
			t.Fatal(err)
		}
	}
	settled, err := store.ReviewRun(ctx, runID)
	if err != nil || settled.State != spine.AgentRunInterrupted || settled.ClaimEvidenceID != "" || settled.ObservedEvidenceID != "" || !strings.Contains(settled.Attention, "resources are being reclaimed") {
		t.Fatalf("interrupted isolated reviewer=%#v err=%v", settled, err)
	}
}

func prepareReviewBoundaryIntegration(t *testing.T, store postgres.Store, run spine.AgentRun) {
	t.Helper()
	ctx := context.Background()
	prepareReviewResourcesIntegration(t, store, run)
	session, err := store.BeginReviewSession(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.State != spine.ActionSucceeded {
		controllerID := spine.ReviewControllerID(run.ID, run.ReviewerSandboxID, run.ReviewerOwnerNonce)
		if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "session-" + run.ID, Outcome: "foreign-review-controller"}); err == nil {
			t.Fatal("foreign logical reviewer controller identity was accepted")
		}
		if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "session-" + run.ID, Outcome: controllerID}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PrepareAgentRun(ctx, run.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, run.ID, "turn-"+run.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	tree := strings.Repeat("d", 40)
	if err := store.RecordReviewPostState(ctx, run.ID, spine.Receipt{ExternalID: run.Workspace, Outcome: run.Revision + " " + tree + " clean"}); err != nil {
		t.Fatal(err)
	}
}

func prepareReviewResourcesIntegration(t *testing.T, store postgres.Store, run spine.AgentRun) {
	t.Helper()
	ctx := context.Background()
	sandbox, err := store.BeginReviewSandbox(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.State != spine.ActionSucceeded {
		if err := store.CompleteAction(ctx, sandbox.ID, spine.Receipt{ExternalID: run.ReviewerSandboxID, Outcome: run.Revision}); err != nil {
			t.Fatal(err)
		}
	}
	tree := strings.Repeat("d", 40)
	workspace, err := store.BeginReviewWorkspace(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.State != spine.ActionSucceeded {
		if err := store.CompleteAction(ctx, workspace.ID, spine.Receipt{ExternalID: run.Workspace, Outcome: run.Revision + " " + tree + " clean"}); err != nil {
			t.Fatal(err)
		}
	}
	route, err := store.BeginReviewRoute(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route.State != spine.ActionSucceeded {
		if err := store.CompleteAction(ctx, route.ID, spine.Receipt{ExternalID: "route-" + run.ID, Outcome: run.ReviewerSandboxID}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUnknownDeclaredPerformancePersistsTriageWithMandatoryFloor(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, verifiedID := prepareReviewIntegrationJob(t, store, "unknown-performance-triage")
	if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
		t.Fatal(err)
	}
	record, created, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision})
	if err != nil || !created {
		t.Fatalf("activation=%#v created=%t err=%v", record, created, err)
	}
	facts, err := policy.FactsFromPaths(job.StartingRevision, revision, []string{"internal/cache/cache.go"}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	record.Facts, record.Initial, record.Final = facts, plan, plan
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.ReviewPlan(ctx, job.ID, revision)
	updated, jobErr := store.Job(ctx, job.ID)
	runs, runsErr := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || jobErr != nil || runsErr != nil || persisted.State != "triage-pending" || persisted.Initial.Decision != "triage" || !slices.Equal(persisted.Initial.Roles, []policy.Role{policy.RolePerformance}) || updated.WorkflowPhase != "review-triage" || len(runs) != 1 || runs[0].Role != spine.ReviewTriageRole {
		t.Fatalf("plan=%#v Job=%#v runs=%#v err=%v jobErr=%v runsErr=%v", persisted, updated, runs, err, jobErr, runsErr)
	}
}

func TestRepairedActivationRejectsOptionalRolesAndPersistsTargetedFloor(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, verifiedID := prepareReviewIntegrationJob(t, store, "repaired-targeted-floor")
	if _, err := db.ExecContext(ctx, `update dorf.jobs set review_repair_count=1 where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkChecksVerified(ctx, job.ID, revision, []string{verifiedID}); err != nil {
		t.Fatal(err)
	}
	requested := []policy.Role{policy.RoleBrowserUI, policy.RoleCriticalBoundary, policy.RolePerformance}
	if _, _, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision, RequestedRoles: requested}); err == nil || !strings.Contains(err.Error(), "cannot replay optional requested Roles") {
		t.Fatalf("repaired optional activation error=%v", err)
	}
	if _, err := store.ReviewPlan(ctx, job.ID, revision); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected activation persisted a plan: %v", err)
	}
	waiting, err := store.Job(ctx, job.ID)
	if err != nil || waiting.WorkflowPhase != "review-activation" {
		t.Fatalf("rejected activation mutated Job=%#v err=%v", waiting, err)
	}
	record, created, err := store.ActivateReview(ctx, spine.ReviewActivation{JobID: job.ID, Revision: revision})
	if err != nil || !created || len(record.RequestedRoles) != 0 || record.RequestedByRunID != "" {
		t.Fatalf("empty repaired activation=%#v created=%t err=%v", record, created, err)
	}
	facts, err := policy.FactsFromPaths(job.StartingRevision, revision, []string{"internal/auth/session.go"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	floor, err := policy.ReviewPolicy(facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	targeted, err := policy.TargetedReverification(floor, []policy.Role{policy.RoleCriticalBoundary})
	if err != nil {
		t.Fatal(err)
	}
	record.Facts, record.Initial, record.Final = facts, targeted, targeted
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.ReviewPlan(ctx, job.ID, revision)
	runs, runsErr := store.ReviewRuns(ctx, job.ID, revision)
	wantRoles := []policy.Role{policy.RoleAuthAuthority, policy.RoleCriticalBoundary}
	wantReasons := []policy.Reason{
		{Role: policy.RoleAuthAuthority, Source: "mandatory", Detail: "authentication or authority paths changed"},
		{Role: policy.RoleCriticalBoundary, Source: "accepted-finding", Detail: "accepted material finding invalidated this Role's claim"},
	}
	if err != nil || runsErr != nil || !slices.Equal(persisted.Initial.Roles, wantRoles) || !slices.Equal(persisted.Initial.Reasons, wantReasons) || len(runs) != len(wantRoles) {
		t.Fatalf("targeted plan=%#v runs=%#v err=%v runsErr=%v", persisted, runs, err, runsErr)
	}
	for _, reason := range persisted.Initial.Reasons {
		if reason.Source == "implementation-request" {
			t.Fatalf("targeted plan retained optional provenance: %#v", persisted.Initial.Reasons)
		}
	}
}

func prepareReviewIntegrationJob(t *testing.T, store postgres.Store, suffix string) (spine.Job, string, string) {
	t.Helper()
	ctx := context.Background()
	start, revision := strings.Repeat("a", 40), strings.Repeat("b", 40)
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: fmt.Sprintf("review-policy-%s-%d", strings.ReplaceAll(suffix, " ", "-"), time.Now().UnixNano()), Goal: "bounded implementation", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/review-policy", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	setup, err := store.BeginSetup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSetup(ctx, setup.ID, integrationEvidence(setup.ID, "repository-setup", setup.ID, "", start, "1"), spine.CommandObservation{Command: "prepare", StartedAt: now, FinishedAt: now}, []spine.DeclaredCheck{{Name: "check", Command: "go test ./..."}}); err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "session-" + job.ID}); err != nil {
		t.Fatal(err)
	}
	completeNextIntegrationRun(t, store, job.ID, "session-"+job.ID, "turn-"+job.ID)
	commit, started, err := store.BeginCommit(ctx, job.ID, start)
	if err != nil || !started {
		t.Fatalf("commit=%#v started=%v err=%v", commit, started, err)
	}
	if err := store.RecordRevision(ctx, commit.ID, spine.CommitObservation{Parent: start, Revision: revision, Tree: strings.Repeat("c", 40), Branch: job.Branch}, integrationEvidence(commit.ID, "git-revision", commit.ID, "", revision, "2")); err != nil {
		t.Fatal(err)
	}
	check, err := store.BeginCheck(ctx, job.ID, revision, "check", "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	checkEvidence := integrationEvidence(check.ID, "check-output", "", check.ID, revision, "3")
	if err := store.RecordCheck(ctx, check, checkEvidence, spine.CommandObservation{Command: check.Command, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return job, revision, checkEvidence.ID
}

func TestFailedSetupRetryPreservesTerminalEvidenceAndSelectsNewAction(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	start := strings.Repeat("7", 40)
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: fmt.Sprintf("setup-retry-%d", time.Now().UnixNano()), Goal: "bounded setup retry", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/setup-retry", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Round(time.Microsecond)
	failed, err := store.BeginSetup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	failedEvidence := integrationEvidence(failed.ID, "repository-setup", failed.ID, "", start, "8")
	observation := spine.CommandObservation{Command: "go mod download", ExitCode: 127, StartedAt: now, FinishedAt: now}
	declared := []spine.DeclaredCheck{{Name: "check", Command: "go test ./..."}}
	collidingPublic, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "setup-retry:toolchain-repair-1", Input: "ordinary caller input"})
	if err != nil || !created || collidingPublic.Sequence != 2 {
		t.Fatalf("public collision setup=%#v created=%v err=%v", collidingPublic, created, err)
	}
	if err := store.RecordSetup(ctx, failed.ID, failedEvidence, observation, declared); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.RetrySetup(ctx, job.ID, "invalid-no-wake", " "); err == nil {
		t.Fatal("setup retry admitted an Action without a valid durable wake")
	}
	job, err = store.Job(ctx, job.ID)
	actions, actionsErr := store.Actions(ctx, job.ID)
	invalidActionID := spine.ScopedActionID(job.ID, spine.ActionRepositorySetup, "invalid-no-wake")
	invalidActionPresent := false
	for _, action := range actions {
		invalidActionPresent = invalidActionPresent || action.ID == invalidActionID
	}
	if err != nil || actionsErr != nil || job.WorkflowPhase != "blocked" || invalidActionPresent {
		t.Fatalf("invalid retry mutated Job=%#v Actions=%#v err=%v actionsErr=%v", job, actions, err, actionsErr)
	}

	retry, wake, retryCreated, err := workflow.RetrySetup(ctx, store, client, job.ID, "toolchain-repair-1", "operator repaired the declared toolchain")
	if err != nil || !retryCreated || retry.ID == failed.ID || retry.Scope != "toolchain-repair-1" || wake.Sequence != 3 {
		t.Fatalf("retry=%#v wake=%#v created=%v err=%v", retry, wake, retryCreated, err)
	}
	nextWake, err := store.NextWakeSequence(ctx, job.ID)
	if err != nil || nextWake != wake.Sequence {
		t.Fatalf("setup retry race selected wake sequence %d, want retry sequence %d: %v", nextWake, wake.Sequence, err)
	}
	repeated, repeatedWake, retryCreated, err := workflow.RetrySetup(ctx, store, client, job.ID, " toolchain-repair-1 ", "operator repaired the declared toolchain")
	if err != nil || retryCreated || repeated != retry || repeatedWake != wake {
		t.Fatalf("idempotent retry=%#v wake=%#v created=%v err=%v", repeated, repeatedWake, retryCreated, err)
	}
	if err := store.RecordSetup(ctx, failed.ID, failedEvidence, observation, declared); err == nil {
		t.Fatal("superseded failed setup Action was allowed to mutate the current workflow")
	}
	current, err := store.BeginSetup(ctx, job.ID)
	if err != nil || current.ID != retry.ID {
		t.Fatalf("current setup=%#v err=%v", current, err)
	}
	retryFailedEvidence := integrationEvidence(retry.ID, "repository-setup", retry.ID, "", start, "9")
	if err := store.RecordSetup(ctx, retry.ID, retryFailedEvidence, observation, declared); err != nil {
		t.Fatal(err)
	}
	terminalRetry, terminalWake, retryCreated, err := workflow.RetrySetup(ctx, store, client, job.ID, "toolchain-repair-1", "operator repaired the declared toolchain")
	if err != nil || retryCreated || terminalRetry.ID != retry.ID || terminalRetry.State != spine.ActionFailed || terminalWake != wake {
		t.Fatalf("terminal exact retry=%#v wake=%#v created=%v err=%v", terminalRetry, terminalWake, retryCreated, err)
	}
	secondRetry, secondWake, retryCreated, err := workflow.RetrySetup(ctx, store, client, job.ID, "toolchain-repair-2", "operator repaired the declared toolchain again")
	if err != nil || !retryCreated || secondRetry.ID == retry.ID {
		t.Fatalf("second retry=%#v created=%v err=%v", secondRetry, retryCreated, err)
	}
	supersededRetry, supersededWake, retryCreated, err := workflow.RetrySetup(ctx, store, client, job.ID, "toolchain-repair-1", "operator repaired the declared toolchain")
	if err != nil || retryCreated || supersededRetry.ID != retry.ID || supersededRetry.State != spine.ActionFailed || supersededWake != wake {
		t.Fatalf("superseded exact retry=%#v wake=%#v created=%v err=%v", supersededRetry, supersededWake, retryCreated, err)
	}
	passedEvidence := integrationEvidence(secondRetry.ID, "repository-setup", secondRetry.ID, "", start, "a")
	observation.ExitCode = 0
	if err := store.RecordSetup(ctx, secondRetry.ID, passedEvidence, observation, declared); err != nil {
		t.Fatal(err)
	}
	job, err = store.Job(ctx, job.ID)
	if err != nil || job.WorkflowPhase != "implementing" {
		t.Fatalf("recovered Job=%#v err=%v", job, err)
	}
	records, err := store.Evidence(ctx, job.ID)
	if err != nil || len(records) != 3 || records[0].ID != failedEvidence.ID || records[1].ID != retryFailedEvidence.ID || records[2].ID != passedEvidence.ID {
		t.Fatalf("retained setup Evidence=%#v err=%v", records, err)
	}
	if _, _, _, err := store.RetrySetup(ctx, job.ID, "unjustified-second-retry", "not justified after success"); err == nil {
		t.Fatal("successful setup admitted another retry generation")
	}
	if err := store.CloseAdmission(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	closedRetry, closedWake, retryCreated, err := workflow.RetrySetup(ctx, store, client, job.ID, "toolchain-repair-2", "operator repaired the declared toolchain again")
	if err != nil || retryCreated || closedRetry.ID != secondRetry.ID || closedRetry.State != spine.ActionSucceeded || closedWake != secondWake {
		t.Fatalf("closed exact retry=%#v wake=%#v created=%v err=%v", closedRetry, closedWake, retryCreated, err)
	}
}

func TestCommitAdmissionBoundaryIncludesOrRejectsLateSteeringAtomically(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	start := strings.Repeat("6", 40)
	key := fmt.Sprintf("commit-admission-boundary-%d", time.Now().UnixNano())
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: key, Goal: "bounded implementation", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/commit-boundary", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	setup, err := store.BeginSetup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSetup(ctx, setup.ID, integrationEvidence(setup.ID, "repository-setup", setup.ID, "", start, "f"), spine.CommandObservation{Command: "prepare", StartedAt: now, FinishedAt: now}, []spine.DeclaredCheck{{Name: "check", Command: "go test ./..."}}); err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "session-" + job.ID
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: sessionID}); err != nil {
		t.Fatal(err)
	}
	completeNextIntegrationRun(t, store, job.ID, sessionID, "turn-initial-"+job.ID)
	if delivery, err := store.NextDelivery(ctx, job.ID, sessionID); err != nil || delivery != nil {
		t.Fatalf("pre-boundary delivery=%#v err=%v", delivery, err)
	}

	late, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "late-before-commit", Input: "include this bounded steering"})
	if err != nil || !created {
		t.Fatalf("late admission=%#v created=%v err=%v", late, created, err)
	}
	if action, started, err := store.BeginCommit(ctx, job.ID, start); err != nil || started || action.ID != "" {
		t.Fatalf("commit crossed admitted FIFO action=%#v started=%v err=%v", action, started, err)
	}
	completeNextIntegrationRun(t, store, job.ID, sessionID, "turn-late-"+job.ID)
	action, started, err := store.BeginCommit(ctx, job.ID, start)
	if err != nil || !started || action.ID == "" {
		t.Fatalf("commit reservation action=%#v started=%v err=%v", action, started, err)
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "late-after-commit", Input: "must not run"}); err == nil || !strings.Contains(err.Error(), "no longer accepts implementation steering") {
		t.Fatalf("post-boundary admission error=%v", err)
	}
	retry, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: late.CallerID, Input: late.Input})
	if err != nil || created || retry != late {
		t.Fatalf("idempotent admitted retry=%#v created=%v err=%v", retry, created, err)
	}
}

func integrationEvidence(owner, kind, actionID, checkID, revision, digestByte string) spine.Evidence {
	now := time.Now().UTC().Round(time.Microsecond)
	return spine.Evidence{ID: spine.EvidenceID(owner, kind), Digest: strings.Repeat(digestByte, 64), ByteSize: 10, MediaType: "application/vnd.dorf.observation+json", Producer: "integration-test", Provenance: "observed", Kind: kind, ActionID: actionID, CheckID: checkID, Revision: revision, StartedAt: now, FinishedAt: now}
}

func completeNextIntegrationRun(t *testing.T, store postgres.Store, jobID, sessionID, turnID string) {
	t.Helper()
	delivery, err := store.NextDelivery(context.Background(), jobID, sessionID)
	if err != nil || delivery == nil {
		t.Fatalf("next delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(context.Background(), delivery.AgentRun.ID, delivery.AgentRun.BaselineTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(context.Background(), delivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(context.Background(), delivery.AgentRun.ID, turnID, "completed"); err != nil {
		t.Fatal(err)
	}
}

func TestAbsurdDistinctMessageWakesResumeSeparateIdleCyclesInFIFO(t *testing.T) {
	db, store, unusedClient := testDatabase(t)
	unusedClient.Close()
	externals := &integrationExternals{}
	client, err := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	workflow.Register(client, spine.Service{Store: store, Externals: externals}, store)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	job, created, err := workflow.Admit(ctx, store, client, postgres.NewJob{AdmissionKey: "wake-cycles-" + suffix, Goal: "initial", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/wake-cycles", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
	if err != nil || !created {
		t.Fatalf("admit Job created=%v err=%v", created, err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, job.TaskID) })
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "wake-before-" + suffix, BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.FetchTaskResult(ctx, config.QueueName, job.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State == absurd.TaskCompleted {
		t.Fatal("task completed before FIFO sequence 2 was admitted")
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[1]" {
		t.Fatalf("initial execution order=%v want=[1]", got)
	}

	// Simulate the honest admission/event crash window: PostgreSQL commits the
	// message, then the process disappears before emitting its wake. The same
	// caller retry must return the same row and repair the same wake identity.
	second, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "second", Input: "second"})
	if err != nil || !created || second.Sequence != 2 {
		t.Fatalf("crash-window admission=%#v created=%v err=%v", second, created, err)
	}
	nextWake, err := store.NextWakeSequence(ctx, job.ID)
	if err != nil || nextWake != second.Sequence {
		t.Fatalf("idle/admission race selected wake sequence %d, want pending sequence %d: %v", nextWake, second.Sequence, err)
	}
	retried, created, err := workflow.AdmitMessage(ctx, store, client, postgres.NewMessage{JobID: job.ID, CallerID: "second", Input: "second"})
	if err != nil || created || retried != second {
		t.Fatalf("wake-repair retry=%#v created=%v err=%v", retried, created, err)
	}
	retriedAgain, created, err := workflow.AdmitMessage(ctx, store, client, postgres.NewMessage{JobID: job.ID, CallerID: "second", Input: "second"})
	if err != nil || created || retriedAgain != second {
		t.Fatalf("repeated wake repair=%#v created=%v err=%v", retriedAgain, created, err)
	}
	var secondWakeRows int
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.e_dorf_jobs where event_name=$1`, workflow.WakeEvent(job.ID, second.Sequence)).Scan(&secondWakeRows); err != nil {
		t.Fatal(err)
	}
	if secondWakeRows != 1 {
		t.Fatalf("idempotent admission retry produced %d sequence-2 wake identities", secondWakeRows)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "wake-second-" + suffix, BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = client.FetchTaskResult(ctx, config.QueueName, job.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State == absurd.TaskCompleted {
		t.Fatal("sequence 2 wake skipped the separate idle wait for sequence 3")
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("second execution order=%v want=[1 2]", got)
	}
	third, created, err := workflow.AdmitMessage(ctx, store, client, postgres.NewMessage{JobID: job.ID, CallerID: "third", Input: "third"})
	if err != nil || !created || third.Sequence != 3 {
		t.Fatalf("third admission=%#v created=%v err=%v", third, created, err)
	}
	var distinctWakeRows int
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.e_dorf_jobs where event_name in ($1,$2)`, workflow.WakeEvent(job.ID, second.Sequence), workflow.WakeEvent(job.ID, third.Sequence)).Scan(&distinctWakeRows); err != nil {
		t.Fatal(err)
	}
	if distinctWakeRows != 2 {
		t.Fatalf("separate admitted FIFO positions produced %d distinct wake identities", distinctWakeRows)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "wake-third-" + suffix, BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	snapshot, err = client.FetchTaskResult(ctx, config.QueueName, job.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State == absurd.TaskCompleted {
		t.Fatal("task completed instead of returning to the sequence 4 idle wait")
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("third execution order=%v want=[1 2 3]", got)
	}
}

func TestCleanupRecoversCompletedNativeTurnAfterRunTaskExhaustion(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	job, created, err := workflow.Admit(ctx, store, client, postgres.NewJob{AdmissionKey: "cleanup-exhausted-" + suffix, Goal: "initial", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/cleanup-exhausted", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
	if err != nil || !created {
		t.Fatalf("admit Job created=%v err=%v", created, err)
	}
	sandbox, err := store.BeginAction(ctx, job.ID, spine.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, sandbox.ID, spine.Receipt{ExternalID: spine.MainSandboxName(job.ID)}); err != nil {
		t.Fatal(err)
	}
	route, err := store.BeginAction(ctx, job.ID, spine.ActionRouteCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, route.ID, spine.Receipt{ExternalID: spine.ProviderRouteID(route.ID)}); err != nil {
		t.Fatal(err)
	}
	taskIDs := []string{job.TaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})
	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: "cleanup-session-" + suffix}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.NextDelivery(ctx, job.ID, "cleanup-session-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	turnID := "cleanup-turn-" + suffix
	if err := store.BindNativeTurn(ctx, delivery.AgentRun.ID, turnID, "running"); err != nil {
		t.Fatal(err)
	}
	second, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "later-pending", Input: "must not be submitted by cleanup"})
	if err != nil || !created || second.Sequence != 2 {
		t.Fatalf("later message=%#v created=%v err=%v", second, created, err)
	}
	if _, err := db.ExecContext(ctx, `update absurd.r_dorf_jobs set state='failed',claimed_by=null,claim_expires_at=null where task_id=$1::uuid`, job.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update absurd.t_dorf_jobs set state='failed',attempts=5 where task_id=$1::uuid`, job.TaskID); err != nil {
		t.Fatal(err)
	}

	externals := &integrationExternals{turns: []spine.NativeTurn{{ID: turnID, Status: "completed"}}, submitted: []int64{1}}
	cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskIDs = append(taskIDs, cleaning.CleanupTaskID)
	if cleaning.AdmissionOpen || cleaning.RunTerminalState != "failed" {
		t.Fatalf("cleanup did not close admission and retain failed run truth: %#v", cleaning)
	}
	if err := (spine.Service{Store: store, Externals: externals}).Cleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := (spine.Service{Store: store, Externals: externals}).Cleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.CleanupState != spine.CleanupComplete || cleaned.RunTerminalState != "failed" {
		t.Fatalf("cleaned Job=%#v", cleaned)
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].State != spine.AgentRunCompleted || messages[0].NativeTurnID != turnID || messages[1].State != spine.AgentRunPending || messages[1].NativeTurnID != "" {
		t.Fatalf("cleanup delivery truth=%#v", messages)
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[1]" {
		t.Fatalf("cleanup submitted pending FIFO input: %v", got)
	}
	if got := externals.effectKinds(); fmt.Sprint(got) != "[provider-route-revoke sandbox-delete]" {
		t.Fatalf("cleanup effects=%v", got)
	}
	evidence, err := store.TaskEvidence(ctx, job.TaskID)
	if err != nil || evidence.State != "failed" || evidence.Attempts != 5 {
		t.Fatalf("failed run evidence=%#v err=%v", evidence, err)
	}
}

type integrationExternals struct {
	mu        sync.Mutex
	turns     []spine.NativeTurn
	submitted []int64
	effects   []spine.ActionKind
}

func (e *integrationExternals) receipt(job spine.Job, action spine.Action) (spine.Receipt, error) {
	e.mu.Lock()
	e.effects = append(e.effects, action.Kind)
	e.mu.Unlock()
	return spine.Receipt{ExternalID: "integration-" + string(action.Kind) + "-" + job.ID}, nil
}
func (e *integrationExternals) SandboxCreate(_ context.Context, job spine.Job, action spine.Action) (spine.Receipt, error) {
	return e.receipt(job, action)
}
func (e *integrationExternals) RepositoryClone(_ context.Context, job spine.Job, action spine.Action) (spine.Receipt, error) {
	return e.receipt(job, action)
}
func (e *integrationExternals) RouteCreate(_ context.Context, job spine.Job, action spine.Action) (spine.Receipt, error) {
	return e.receipt(job, action)
}
func (e *integrationExternals) AgentInitialTurn(_ context.Context, job spine.Job, delivery spine.Delivery) (string, spine.NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.turns) == 0 {
		turn := spine.NativeTurn{ID: "integration-turn-" + delivery.Message.ID, Status: "running"}
		e.submitted = append(e.submitted, delivery.Message.Sequence)
		e.turns = append(e.turns, turn)
	}
	return "integration-session-" + job.ID, e.turns[0], nil
}
func (e *integrationExternals) AgentInitialTurns(_ context.Context, job spine.Job) (string, []spine.NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return "integration-session-" + job.ID, append([]spine.NativeTurn(nil), e.turns...), nil
}
func (e *integrationExternals) AgentTurns(_ context.Context, _ spine.Job, _ string) ([]spine.NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]spine.NativeTurn(nil), e.turns...), nil
}
func (e *integrationExternals) AgentSubmit(_ context.Context, _ spine.Job, delivery spine.Delivery) (spine.NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	turn := spine.NativeTurn{ID: "integration-turn-" + delivery.Message.ID, Status: "running"}
	e.submitted = append(e.submitted, delivery.Message.Sequence)
	e.turns = append(e.turns, turn)
	return turn, nil
}
func (e *integrationExternals) AgentWait(_ context.Context, _ spine.Job, _ string, turnID string) (spine.NativeTurn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for index := range e.turns {
		if e.turns[index].ID == turnID {
			e.turns[index].Status = "completed"
		}
	}
	return spine.NativeTurn{ID: turnID, Status: "completed"}, nil
}
func (e *integrationExternals) RouteRevoke(_ context.Context, job spine.Job, action spine.Action) (spine.Receipt, error) {
	e.mu.Lock()
	e.effects = append(e.effects, action.Kind)
	e.mu.Unlock()
	return spine.Receipt{ExternalID: job.RouteID, Outcome: "revoked"}, nil
}
func (e *integrationExternals) SandboxDelete(_ context.Context, job spine.Job, action spine.Action) (spine.Receipt, error) {
	e.mu.Lock()
	e.effects = append(e.effects, action.Kind)
	e.mu.Unlock()
	return spine.Receipt{ExternalID: job.SandboxID, Outcome: "deleted"}, nil
}
func (e *integrationExternals) submittedSequences() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int64(nil), e.submitted...)
}

func (e *integrationExternals) effectKinds() []spine.ActionKind {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]spine.ActionKind(nil), e.effects...)
}

func TestMigration008UpgradesApplied001Through007(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	sourceConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConfig := sourceConfig.Copy()
	adminConfig.Database = "postgres"
	admin, err := sql.Open("pgx", adminConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	databaseName := fmt.Sprintf("dorf_delivery_upgrade_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), `create database `+pgx.Identifier{databaseName}.Sanitize()+` template `+pgx.Identifier{sourceConfig.Database}.Sanitize()); err != nil {
		t.Fatalf("create isolated delivery upgrade database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `select pg_terminate_backend(pid) from pg_stat_activity where datname=$1 and pid<>pg_backend_pid()`, databaseName); err != nil {
			t.Errorf("terminate isolated delivery upgrade connections: %v", err)
		}
		if _, err := admin.ExecContext(context.Background(), `drop database `+pgx.Identifier{databaseName}.Sanitize()); err != nil {
			t.Errorf("drop isolated delivery upgrade database: %v", err)
		}
	})
	upgradeConfig := sourceConfig.Copy()
	upgradeConfig.Database = databaseName
	db, err := sql.Open("pgx", upgradeConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `drop schema if exists dorf cascade`); err != nil {
		t.Fatal(err)
	}
	prior := []string{"001_dorf.sql", "002_run_terminal.sql", "003_exactly_once_messages.sql", "004_revision_evidence.sql", "005_commit_admission.sql", "006_setup_retry.sql", "007_review_policy.sql"}
	for _, name := range prior {
		contents, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(contents)); err != nil {
			t.Fatalf("apply prior migration %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx, `insert into dorf.schema_migrations(name) values($1)`, name); err != nil {
			t.Fatal(err)
		}
	}
	var deliveryColumn bool
	if err := db.QueryRowContext(ctx, `select exists(select 1 from information_schema.columns where table_schema='dorf' and table_name='job_messages' and column_name='delivery_intent')`).Scan(&deliveryColumn); err != nil {
		t.Fatal(err)
	}
	if deliveryColumn {
		t.Fatal("delivery schema was already present after recorded migrations 001 through 007")
	}
	jobID := "job-delivery-upgrade"
	if _, err := db.ExecContext(ctx, `insert into dorf.jobs(id,admission_key,goal,repository,revision,starting_revision,branch,provider_connection,model,reasoning_effort) values($1,'delivery-upgrade','upgrade delivery schema','https://github.com/aphronio/dorf.git',$2,$2,'dorf/delivery-upgrade','primary','gpt-5.6-sol','high')`, jobID, strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.job_messages(id,job_id,caller_id,sequence,input) values('message-delivery-upgrade',$1,'existing-message',1,'preserve me')`, jobID); err != nil {
		t.Fatal(err)
	}
	store := postgres.Store{DB: db}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var intent, target string
	if err := db.QueryRowContext(ctx, `select delivery_intent,coalesce(steer_target_turn_id,'') from dorf.job_messages where id='message-delivery-upgrade'`).Scan(&intent, &target); err != nil {
		t.Fatal(err)
	}
	if intent != string(spine.MessageFollow) || target != "" {
		t.Fatalf("upgraded existing message intent=%q target=%q", intent, target)
	}
	var migrationApplied, nativeTurnUnique bool
	if err := db.QueryRowContext(ctx, `select exists(select 1 from dorf.schema_migrations where name='008_message_delivery.sql'),exists(select 1 from pg_constraint where conrelid='dorf.agent_runs'::regclass and conname='agent_runs_native_turn_id_key')`).Scan(&migrationApplied, &nativeTurnUnique); err != nil {
		t.Fatal(err)
	}
	if !migrationApplied || nativeTurnUnique {
		t.Fatalf("migration applied=%v old native-turn uniqueness retained=%v", migrationApplied, nativeTurnUnique)
	}
	if _, err := db.ExecContext(ctx, `update dorf.job_messages set delivery_intent='steer' where id='message-delivery-upgrade'`); err == nil {
		t.Fatal("delivery target invariant allowed steer without an exact target")
	}
	if _, err := db.ExecContext(ctx, `update dorf.job_messages set delivery_intent='steer',steer_target_turn_id='turn-active' where id='message-delivery-upgrade'`); err != nil {
		t.Fatal(err)
	}
}

func TestMigration003PreservesCompletedGoJobFacts(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	sourceConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if sourceConfig.Database == "" {
		t.Fatal("DORF_TEST_DATABASE_URL must name a database for the isolated upgrade clone")
	}
	adminConfig := sourceConfig.Copy()
	adminConfig.Database = "postgres"
	admin, err := sql.Open("pgx", adminConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	databaseName := fmt.Sprintf("dorf_upgrade_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), `create database `+pgx.Identifier{databaseName}.Sanitize()+` template `+pgx.Identifier{sourceConfig.Database}.Sanitize()); err != nil {
		t.Fatalf("create isolated upgrade database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), `select pg_terminate_backend(pid) from pg_stat_activity where datname=$1 and pid<>pg_backend_pid()`, databaseName); err != nil {
			t.Errorf("terminate isolated upgrade database connections: %v", err)
		}
		if _, err := admin.ExecContext(context.Background(), `drop database `+databaseName); err != nil {
			t.Errorf("drop isolated upgrade database: %v", err)
		}
	})
	upgradeConfig := sourceConfig.Copy()
	upgradeConfig.Database = databaseName
	db, err := sql.Open("pgx", upgradeConfig.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `drop schema if exists dorf cascade`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `truncate absurd.c_dorf_jobs,absurd.w_dorf_jobs,absurd.r_dorf_jobs,absurd.e_dorf_jobs,absurd.t_dorf_jobs`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_dorf.sql", "002_run_terminal.sql"} {
		contents, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(contents)); err != nil {
			t.Fatalf("apply prior schema %s: %v", name, err)
		}
	}
	observedAt := time.Date(2026, 8, 6, 12, 34, 56, 0, time.UTC)
	input := postgres.NewJob{AdmissionKey: "upgrade-preserved", Goal: "completed legacy goal", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/upgrade-preserved", ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high"}
	jobID := spine.JobID(input.AdmissionKey)
	client, err := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	client.MustRegister(absurd.Task("dorf-job-spine-v1", func(_ context.Context, params workflow.Params) (workflow.Result, error) {
		return workflow.Result{JobID: params.JobID, Outcome: "observed"}, nil
	}))
	legacyTask, err := client.Spawn(ctx, "dorf-job-spine-v1", workflow.Params{JobID: jobID}, absurd.SpawnOptions{IdempotencyKey: "run:" + jobID})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "legacy-upgrade", BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	legacySnapshot, err := client.FetchTaskResult(ctx, config.QueueName, legacyTask.TaskID)
	if err != nil || legacySnapshot.State != absurd.TaskCompleted {
		t.Fatalf("legacy v1 task state=%v err=%v", legacySnapshot, err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.jobs(id,admission_key,goal,repository,revision,branch,provider_connection,model,reasoning_effort,state,task_id,native_outcome,observed_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9,'observed',$10,'completed',$11)`, jobID, input.AdmissionKey, input.Goal, input.Repository, input.Revision, input.Branch, input.ProviderConnection, input.Model, input.ReasoningEffort, legacyTask.TaskID, observedAt); err != nil {
		t.Fatal(err)
	}
	actionIDs := map[spine.ActionKind]string{
		spine.ActionSandboxCreate:   "legacy-action-sandbox",
		spine.ActionRepositoryClone: "legacy-action-clone",
		spine.ActionRouteCreate:     "legacy-action-route",
		spine.ActionSessionStart:    "legacy-action-session",
		spine.ActionTurnStart:       "legacy-action-turn",
	}
	externalIDs := map[spine.ActionKind]string{
		spine.ActionSandboxCreate:   "legacy-sandbox",
		spine.ActionRepositoryClone: "legacy-checkout",
		spine.ActionRouteCreate:     "legacy-route",
		spine.ActionSessionStart:    "legacy-session",
		spine.ActionTurnStart:       "legacy-turn",
	}
	for _, kind := range []spine.ActionKind{spine.ActionSandboxCreate, spine.ActionRepositoryClone, spine.ActionRouteCreate, spine.ActionSessionStart, spine.ActionTurnStart} {
		if _, err := db.ExecContext(ctx, `insert into dorf.actions(id,job_id,kind,state,external_id,external_outcome) values($1,$2,$3,'succeeded',$4,$5)`, actionIDs[kind], jobID, kind, externalIDs[kind], map[bool]string{true: "completed", false: ""}[kind == spine.ActionTurnStart]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.sandboxes(job_id,action_id,incus_name,state) values($1,$2,$3,'created')`, jobID, actionIDs[spine.ActionSandboxCreate], externalIDs[spine.ActionSandboxCreate]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.routes(job_id,action_id,route_id,state) values($1,$2,$3,'active')`, jobID, actionIDs[spine.ActionRouteCreate], externalIDs[spine.ActionRouteCreate]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.sessions(job_id,action_id,native_session_id,observed_at) values($1,$2,$3,$4)`, jobID, actionIDs[spine.ActionSessionStart], externalIDs[spine.ActionSessionStart], observedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into dorf.agent_runs(id,job_id,action_id,session_id,native_turn_id,role,native_outcome,observed_at) values('legacy-agent-run',$1,$2,$3,$4,'implement','completed',$5)`, jobID, actionIDs[spine.ActionTurnStart], externalIDs[spine.ActionSessionStart], externalIDs[spine.ActionTurnStart], observedAt); err != nil {
		t.Fatal(err)
	}

	store := postgres.Store{DB: db}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	job, err := store.Job(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != spine.JobObserved || job.SessionID != "legacy-session" || job.SandboxID != "legacy-sandbox" || job.RouteID != "legacy-route" {
		t.Fatalf("upgraded Job lost readable setup/session facts: %#v", job)
	}
	messages, err := store.Messages(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Sequence != 1 || messages[0].AgentRunID != "legacy-agent-run" || messages[0].NativeTurnID != "legacy-turn" || messages[0].NativeOutcome != "completed" || messages[0].State != spine.AgentRunCompleted {
		t.Fatalf("upgraded turn facts=%#v", messages)
	}
	actions, err := store.Actions(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != len(actionIDs) {
		t.Fatalf("upgraded actions=%#v", actions)
	}
	for _, action := range actions {
		if action.ID != actionIDs[action.Kind] || action.ExternalID != externalIDs[action.Kind] || action.State != spine.ActionSucceeded {
			t.Fatalf("upgraded Action changed identity/outcome: %#v", action)
		}
	}
	var jobOutcome string
	var jobObservedAt, runObservedAt time.Time
	if err := db.QueryRowContext(ctx, `select native_outcome,observed_at from dorf.jobs where id=$1`, jobID).Scan(&jobOutcome, &jobObservedAt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `select observed_at from dorf.agent_runs where id='legacy-agent-run'`).Scan(&runObservedAt); err != nil {
		t.Fatal(err)
	}
	if jobOutcome != "completed" || !jobObservedAt.Equal(observedAt) || !runObservedAt.Equal(observedAt) {
		t.Fatalf("upgrade changed durable observation outcome/timestamps: %q %s %s", jobOutcome, jobObservedAt, runObservedAt)
	}
	externals := &integrationExternals{}
	workflow.Register(client, spine.Service{Store: store, Externals: externals}, store)
	if err := store.CheckMessageTaskAttachment(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, workflow.RunTaskName, workflow.Params{JobID: jobID}, absurd.SpawnOptions{IdempotencyKey: postgres.MessageTaskKey(jobID)})
	if err != nil || !spawned.Created {
		t.Fatalf("spawn upgraded v2 task=%#v err=%v", spawned, err)
	}
	if err := store.AttachMessageTask(ctx, jobID, spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	reattached, err := store.Job(ctx, jobID)
	if err != nil || reattached.ID != jobID || reattached.TaskID == legacyTask.TaskID {
		t.Fatalf("upgraded v2 reattachment=%#v err=%v", reattached, err)
	}
	v2TaskID := reattached.TaskID
	var v2Tasks int
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.t_dorf_jobs where task_name=$1 and params->>'job_id'=$2 and idempotency_key=$3`, workflow.RunTaskName, jobID, postgres.MessageTaskKey(jobID)).Scan(&v2Tasks); err != nil {
		t.Fatal(err)
	}
	if v2Tasks != 1 {
		t.Fatalf("reattachment created %d v2 message tasks", v2Tasks)
	}
	repeated, err := client.Spawn(ctx, workflow.RunTaskName, workflow.Params{JobID: jobID}, absurd.SpawnOptions{IdempotencyKey: postgres.MessageTaskKey(jobID)})
	if err != nil || repeated.Created || repeated.TaskID != v2TaskID {
		t.Fatalf("repeated v2 task=%#v err=%v", repeated, err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "upgraded-v2-idle", BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	message, created, err := workflow.AdmitMessage(ctx, store, client, postgres.NewMessage{JobID: jobID, CallerID: "after-upgrade", Input: "continue through v2"})
	if err != nil || !created || message.Sequence != 2 {
		t.Fatalf("post-upgrade message=%#v created=%v err=%v", message, created, err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "upgraded-v2-message", BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[2]" {
		t.Fatalf("post-upgrade v2 submissions=%v want=[2]", got)
	}
	continuedMessages, err := store.Messages(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(continuedMessages) != 2 || continuedMessages[1].Sequence != 2 || continuedMessages[1].State != spine.AgentRunCompleted {
		t.Fatalf("post-upgrade message did not durably complete through v2: %#v", continuedMessages)
	}
	legacySnapshot, err = client.FetchTaskResult(ctx, config.QueueName, legacyTask.TaskID)
	if err != nil || legacySnapshot.State != absurd.TaskCompleted {
		t.Fatalf("historical v1 task was not retained: state=%v err=%v", legacySnapshot, err)
	}
	v2Snapshot, err := client.FetchTaskResult(ctx, config.QueueName, v2TaskID)
	if err != nil || v2Snapshot.State == absurd.TaskCompleted || v2Snapshot.State == absurd.TaskFailed || v2Snapshot.State == absurd.TaskCancelled {
		t.Fatalf("v2 task is not the live post-upgrade consumer: state=%v err=%v", v2Snapshot, err)
	}
}

func TestMessageTaskReattachmentFailsClosed(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	newInput := func(label string) postgres.NewJob {
		return postgres.NewJob{AdmissionKey: "reattach-safety-" + label + "-" + suffix, Goal: "safety input " + label, Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/reattach-" + label, ProviderConnection: "primary", ProviderGatewayState: "/tmp/dorf-provider-gateway-test", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
	}
	var taskIDs []string
	t.Cleanup(func() {
		for _, taskID := range taskIDs {
			_ = client.CancelTask(context.Background(), config.QueueName, taskID)
		}
	})

	t.Run("active predecessor", func(t *testing.T) {
		input := newInput("active")
		job, created, err := store.Admit(ctx, input)
		if err != nil || !created {
			t.Fatalf("admit created=%v err=%v", created, err)
		}
		legacy, err := client.Spawn(ctx, "dorf-job-spine-v1", workflow.Params{JobID: job.ID}, absurd.SpawnOptions{QueueName: config.QueueName, IdempotencyKey: "run:" + job.ID})
		if err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, legacy.TaskID)
		if err := store.SetTaskID(ctx, job.ID, legacy.TaskID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := workflow.Admit(ctx, store, client, input); err == nil || !strings.Contains(err.Error(), "refusing to replace a nonterminal run") {
			t.Fatalf("active predecessor reattachment error=%v", err)
		}
		var v2Tasks int
		if err := db.QueryRowContext(ctx, `select count(*) from absurd.t_dorf_jobs where idempotency_key=$1`, postgres.MessageTaskKey(job.ID)).Scan(&v2Tasks); err != nil {
			t.Fatal(err)
		}
		if v2Tasks != 0 {
			t.Fatalf("active predecessor allowed %d v2 tasks to spawn", v2Tasks)
		}
	})

	t.Run("unrelated current task", func(t *testing.T) {
		input := newInput("unrelated")
		job, created, err := store.Admit(ctx, input)
		if err != nil || !created {
			t.Fatalf("admit created=%v err=%v", created, err)
		}
		unrelated, err := client.Spawn(ctx, "unrelated-run-task", workflow.Params{JobID: job.ID}, absurd.SpawnOptions{QueueName: config.QueueName, IdempotencyKey: "run:" + job.ID})
		if err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, unrelated.TaskID)
		if err := store.SetTaskID(ctx, job.ID, unrelated.TaskID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := workflow.Admit(ctx, store, client, input); err == nil || !strings.Contains(err.Error(), "expected dorf-job-spine-v1 predecessor") {
			t.Fatalf("unrelated current task error=%v", err)
		}
	})

	t.Run("v2 idempotency collision", func(t *testing.T) {
		input := newInput("collision")
		job, created, err := store.Admit(ctx, input)
		if err != nil || !created {
			t.Fatalf("admit created=%v err=%v", created, err)
		}
		collision, err := client.Spawn(ctx, "unrelated-v2-key-owner", workflow.Params{JobID: job.ID}, absurd.SpawnOptions{QueueName: config.QueueName, IdempotencyKey: postgres.MessageTaskKey(job.ID)})
		if err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, collision.TaskID)
		if _, _, err := workflow.Admit(ctx, store, client, input); err == nil || !strings.Contains(err.Error(), "not the expected dorf-job-messages-v2 consumer") {
			t.Fatalf("v2 key collision error=%v", err)
		}
		stored, err := store.Job(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.TaskID != "" {
			t.Fatalf("v2 key collision overwrote Job task_id with %s", stored.TaskID)
		}
	})
}

func TestPostgresJobFenceSerializesOverlappingClaims(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	firstEntered := make(chan struct{})
	release := make(chan struct{})
	secondEntered := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		errs <- store.WithJobFence(ctx, "job-fence-integration", func() error { close(firstEntered); <-release; return nil })
	}()
	<-firstEntered
	go func() {
		errs <- store.WithJobFence(ctx, "job-fence-integration", func() error { close(secondEntered); return nil })
	}()
	select {
	case <-secondEntered:
		close(release)
		t.Fatal("second claim crossed the PostgreSQL Job execution fence")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
