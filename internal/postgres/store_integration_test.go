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
	_ "github.com/jackc/pgx/v5/stdlib"
)

type providerCheck struct {
	err error
}

func (p providerCheck) Check(context.Context, string) error { return p.err }

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
	workflow.Register(client, spine.Service{Store: store}, store, workflow.ProposalRuntime{})
	t.Cleanup(func() {
		client.Close()
		db.Close()
	})
	return db, store, client
}

func TestPostgresMessageIdempotencyConcurrentFIFOAndLowestUnsettled(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	key := fmt.Sprintf("message-integration-%d", time.Now().UnixNano())
	input := postgres.NewJob{AdmissionKey: key, Goal: "initial input", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/integration", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
	blocked := input
	blocked.AdmissionKey += "-provider-blocked"
	if _, _, err := workflow.Admit(ctx, store, client, providerCheck{err: errors.New("provider is not ready")}, blocked); err == nil {
		t.Fatal("new Job bypassed provider readiness")
	}
	if _, err := store.Job(ctx, spine.JobID(blocked.AdmissionKey)); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("failed provider preflight persisted Job: %v", err)
	}
	job, created, err := workflow.Admit(ctx, store, client, providerCheck{}, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	repeatedJob, created, err := workflow.Admit(ctx, store, client, providerCheck{err: errors.New("Gateway unavailable during retry")}, input)
	if err != nil || created || repeatedJob.ID != job.ID || repeatedJob.TaskID != job.TaskID {
		t.Fatalf("idempotent Job admission=%#v created=%v err=%v", repeatedJob, created, err)
	}
	changedJob := input
	changedJob.Goal = "changed complete input"
	if _, _, err := workflow.Admit(ctx, store, client, providerCheck{err: errors.New("Gateway unavailable during retry")}, changedJob); err == nil {
		t.Fatal("changed complete Job input under the same admission key did not conflict")
	}
	taskIDs := []string{job.TaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})

	first, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "client-retry", Input: "same text"})
	if err != nil || !created || first.Sequence != 2 || first.FromKind != "human" || first.FromID != "client-retry" || first.ID != spine.MessageID(job.ID, "human", "client-retry") {
		t.Fatalf("first message=%#v created=%v err=%v", first, created, err)
	}
	repeated, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "client-retry", Input: "same text"})
	if err != nil || created || repeated != first {
		t.Fatalf("idempotent message=%#v created=%v err=%v", repeated, created, err)
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "client-retry", Input: "changed"}); err == nil {
		t.Fatal("changed input under the same source identity did not conflict")
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "client-retry", Input: "same text "}); err == nil {
		t.Fatal("byte-distinct complete input under the same source identity did not conflict")
	}
	distinct, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "client-distinct", Input: "same text"})
	if err != nil || !created || distinct.ID == first.ID || distinct.Sequence != 3 {
		t.Fatalf("distinct identical message=%#v created=%v err=%v", distinct, created, err)
	}
	crossKind, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "workflow", FromID: distinct.FromID, Input: "same source identity from the workflow"})
	if err != nil || !created || crossKind.Sequence != 4 || crossKind.ID == distinct.ID || crossKind.ID != spine.MessageID(job.ID, "workflow", distinct.FromID) || crossKind.FromKind != "workflow" || crossKind.FromID != distinct.FromID {
		t.Fatalf("cross-kind source identity=%#v created=%v err=%v", crossKind, created, err)
	}

	const concurrent = 12
	sequences := make(chan int64, concurrent)
	errors := make(chan error, concurrent)
	var wg sync.WaitGroup
	for i := range concurrent {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			message, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: fmt.Sprintf("concurrent-%02d", i), Input: "same concurrent text"})
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
		if sequence != i+5 {
			t.Fatalf("concurrent FIFO positions=%v", got)
		}
	}

	threadID := "thread-" + job.ID
	delivery, err := store.NextDelivery(ctx, job.ID)
	if err != nil || delivery.Message.Sequence != 1 {
		t.Fatalf("lowest delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", threadID, "turn-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	next, err := store.NextDelivery(ctx, job.ID)
	if err != nil || next.Message.Sequence != 2 || next.AgentRun.ID == delivery.AgentRun.ID {
		t.Fatalf("next delivery=%#v err=%v", next, err)
	}
	if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", "turn-"+job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-2-"+job.ID, "running"); err != nil {
		t.Fatal(err)
	}
	blocker, err := store.HarnessMutationDelivery(ctx, job.ID)
	if err != nil || blocker == nil || blocker.AgentRun.State != spine.AgentRunActive {
		t.Fatalf("active harness mutation=%#v err=%v", blocker, err)
	}
	stillOpen, err := store.Job(ctx, job.ID)
	if err != nil || !stillOpen.AdmissionOpen {
		t.Fatalf("harness mutation inspection changed admission: %#v err=%v", stillOpen, err)
	}
	if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-2-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
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
		t.Fatalf("cleanup crossed the active harness-mutation fence: %#v", result)
	case <-time.After(100 * time.Millisecond):
	}
	if _, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "during-active", Input: "must be rejected after cleanup closes admission"}); err == nil || created || !strings.Contains(err.Error(), "admission is closed") {
		close(releaseFence)
		t.Fatalf("cleanup did not close admission before waiting for the active harness-mutation fence: created=%v err=%v", created, err)
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
	if retry, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "client-retry", Input: "same text"}); err != nil || created || retry != first {
		t.Fatalf("closed admission did not preserve idempotent retry: %#v %v %v", retry, created, err)
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "after-cleanup", Input: "late"}); err == nil {
		t.Fatal("cleanup allowed a new message")
	}
}

func TestExplicitSteerTargetsAndAcknowledgesExactActiveTurn(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "explicit-steer")
	active, err := store.NextDelivery(ctx, job.ID)
	if err != nil || active == nil {
		t.Fatalf("initial delivery=%#v err=%v", active, err)
	}
	if err := store.PrepareAgentRun(ctx, active.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	activeTurnID := "turn-active-" + job.ID
	if err := store.BindAgentRun(ctx, active.AgentRun.ID, "codex", threadID, activeTurnID, "running"); err != nil {
		t.Fatal(err)
	}

	steerInput := postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "operator-steer", Input: "correct the active work", Intent: spine.MessageSteer}
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
	delivery, err := store.NextDelivery(ctx, job.ID)
	if err != nil || delivery == nil || delivery.Message.ID != steer.ID || delivery.AgentRun.ThreadID != threadID {
		t.Fatalf("steer delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", activeTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, delivery.AgentRun.ID, activeTurnID, "inProgress"); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil || len(messages) != 2 || messages[1].TurnID != activeTurnID || messages[1].Intent != spine.MessageSteer {
		t.Fatalf("steer messages=%#v err=%v", messages, err)
	}
	next, err := store.NextDelivery(ctx, job.ID)
	if err != nil || next == nil || next.Message.ID != active.Message.ID || next.AgentRun.TurnID != activeTurnID {
		t.Fatalf("active follow after steer=%#v err=%v", next, err)
	}
	other, _ := prepareTransportIntegrationJob(t, store, "steer-without-active-turn")
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: other.ID, FromKind: "human", FromID: "invalid-steer", Input: "cannot target", Intent: spine.MessageSteer}); err == nil || !strings.Contains(err.Error(), "exact active regular harness Turn") {
		t.Fatalf("steer without active turn error=%v", err)
	}
}

func TestSharedSteersPersistEveryTerminalTargetOutcome(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			job, threadID := prepareTransportIntegrationJob(t, store, "shared-steer-outcome-"+status)
			target, err := store.NextDelivery(ctx, job.ID)
			if err != nil || target == nil {
				t.Fatalf("target delivery=%#v err=%v", target, err)
			}
			if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
				t.Fatal(err)
			}
			targetTurnID := "turn-shared-" + job.ID
			if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
				t.Fatal(err)
			}
			first, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "first-shared-steer", Input: "first accepted shared input", Intent: spine.MessageSteer})
			if err != nil || !created {
				t.Fatalf("first steer=%#v created=%v err=%v", first, created, err)
			}
			second, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "second-shared-steer", Input: "second accepted shared input", Intent: spine.MessageSteer})
			if err != nil || !created {
				t.Fatalf("second steer=%#v created=%v err=%v", second, created, err)
			}
			firstDelivery, err := store.NextDelivery(ctx, job.ID)
			if err != nil || firstDelivery == nil || firstDelivery.Message.ID != first.ID {
				t.Fatalf("first steer delivery=%#v err=%v", firstDelivery, err)
			}
			if err := store.PrepareAgentRun(ctx, firstDelivery.AgentRun.ID, "codex", targetTurnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindSteer(ctx, firstDelivery.AgentRun.ID, targetTurnID, "inProgress"); err != nil {
				t.Fatal(err)
			}
			secondDelivery, err := store.NextDelivery(ctx, job.ID)
			if err != nil || secondDelivery == nil || secondDelivery.Message.ID != second.ID {
				t.Fatalf("second steer delivery=%#v err=%v", secondDelivery, err)
			}
			if err := store.PrepareAgentRun(ctx, secondDelivery.AgentRun.ID, "codex", targetTurnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindSteer(ctx, secondDelivery.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if err := store.BindSteer(ctx, firstDelivery.AgentRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			messages, err := store.Messages(ctx, job.ID)
			if err != nil || len(messages) != 3 {
				t.Fatalf("messages=%#v err=%v", messages, err)
			}
			for index, message := range messages[1:] {
				if message.Intent != spine.MessageSteer || message.TargetTurnID != targetTurnID || message.TurnID != targetTurnID || message.TurnOutcome != status || message.State != spine.AgentRunCompleted {
					t.Fatalf("shared steer %d=%#v", index+1, message)
				}
			}
		})
	}
}

func TestSteerTerminalFallbackPreservesRequestAndSerializesLaterFIFO(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "steer-terminal-fallback")
	target, err := store.NextDelivery(ctx, job.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%#v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	targetTurnID := "turn-target-" + job.ID
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	steer, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "terminal-race-steer", Input: "preserve exact durable input", Intent: spine.MessageSteer})
	if err != nil || !created || steer.TargetTurnID != targetTurnID {
		t.Fatalf("steer=%#v created=%v err=%v", steer, created, err)
	}
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", threadID, targetTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	fallback, err := store.NextDelivery(ctx, job.ID)
	if err != nil || fallback == nil || fallback.Message.ID != steer.ID || fallback.AgentRun.ID != spine.AgentRunID(steer.ID) {
		t.Fatalf("fallback delivery=%#v err=%v", fallback, err)
	}
	if err := store.PrepareAgentRun(ctx, fallback.AgentRun.ID, "codex", targetTurnID); err != nil {
		t.Fatal(err)
	}
	actualTurnID := "turn-fallback-" + job.ID
	if err := store.BindAgentRun(ctx, fallback.AgentRun.ID, "codex", threadID, actualTurnID, "running"); err != nil {
		t.Fatal(err)
	}
	later, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "later-follow", Input: "later FIFO delivery"})
	if err != nil || !created {
		t.Fatalf("later=%#v created=%v err=%v", later, created, err)
	}
	active, err := store.NextDelivery(ctx, job.ID)
	if err != nil || active == nil || active.Message.ID != steer.ID || active.AgentRun.TurnID != actualTurnID {
		t.Fatalf("active fallback selection=%#v err=%v", active, err)
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil || len(messages) != 3 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	if messages[1].Intent != spine.MessageSteer || messages[1].TargetTurnID != targetTurnID || messages[1].TurnID != actualTurnID {
		t.Fatalf("fallback projection=%#v later=%#v", messages[1], messages[2])
	}
	if err := store.BindAgentRun(ctx, fallback.AgentRun.ID, "codex", threadID, actualTurnID, "failed"); err != nil {
		t.Fatal(err)
	}
	next, err := store.NextDelivery(ctx, job.ID)
	if err != nil || next == nil || next.Message.ID != later.ID || next.AgentRun.ThreadID != threadID {
		t.Fatalf("later delivery=%#v err=%v", next, err)
	}
	messages, err = store.Messages(ctx, job.ID)
	if err != nil || messages[1].State != spine.AgentRunFailed || messages[1].TurnOutcome != "failed" || messages[1].TargetTurnID != targetTurnID {
		t.Fatalf("terminal fallback evidence=%#v err=%v", messages[1], err)
	}
}

func TestAcceptedTerminalTurnAllowsSameThreadFollowFIFO(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			_, store, _ := testDatabase(t)
			ctx := context.Background()
			job, threadID := prepareTransportIntegrationJob(t, store, "terminal-follow-"+status)
			first, err := store.NextDelivery(ctx, job.ID)
			if err != nil || first == nil {
				t.Fatalf("first=%#v err=%v", first, err)
			}
			if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", ""); err != nil {
				t.Fatal(err)
			}
			turnID := "turn-first-" + job.ID
			if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, turnID, "running"); err != nil {
				t.Fatal(err)
			}
			follow, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "queued-follow", Input: "continue after the accepted outcome"})
			if err != nil || !created || follow.Intent != spine.MessageFollow {
				t.Fatalf("follow=%#v created=%v err=%v", follow, created, err)
			}
			stillActive, err := store.NextDelivery(ctx, job.ID)
			if err != nil || stillActive == nil || stillActive.Message.ID != first.Message.ID {
				t.Fatalf("FIFO crossed active turn: delivery=%#v err=%v", stillActive, err)
			}
			if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, turnID, status); err != nil {
				t.Fatal(err)
			}
			next, err := store.NextDelivery(ctx, job.ID)
			if err != nil || next == nil || next.Message.ID != follow.ID || next.AgentRun.ThreadID != threadID {
				t.Fatalf("follow after %s=%#v err=%v", status, next, err)
			}
			if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, "codex", turnID); err != nil {
				t.Fatal(err)
			}
			if err := store.BindAgentRun(ctx, next.AgentRun.ID, "codex", threadID, "turn-follow-"+job.ID, "completed"); err != nil {
				t.Fatal(err)
			}
			messages, err := store.Messages(ctx, job.ID)
			if err != nil || len(messages) != 2 || messages[0].TurnOutcome != status || messages[0].TurnID == "" || messages[1].State != spine.AgentRunCompleted {
				t.Fatalf("preserved %s then follow=%#v err=%v", status, messages, err)
			}
		})
	}
}

func TestSubmittingFollowRemainsDeliveryCandidateUntilReconciled(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "submitting-follow-recovery")
	delivery, err := store.NextDelivery(ctx, job.ID)
	if err != nil || delivery == nil {
		t.Fatalf("initial delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	later, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "later-follow", Input: "must wait for recovery"})
	if err != nil || !created {
		t.Fatalf("later Follow=%#v created=%v err=%v", later, created, err)
	}

	candidate, err := store.DeliveryCandidate(ctx, job.ID)
	if err != nil || candidate == nil || candidate.AgentRun.ID != delivery.AgentRun.ID || candidate.AgentRun.State != spine.AgentRunSubmitting {
		t.Fatalf("submitting candidate=%#v err=%v", candidate, err)
	}
	retry, err := store.NextDelivery(ctx, job.ID)
	if err != nil || retry == nil || retry.AgentRun.ID != delivery.AgentRun.ID || retry.AgentRun.State != spine.AgentRunSubmitting {
		t.Fatalf("submitting retry=%#v err=%v", retry, err)
	}
	if err := store.BindAgentRun(ctx, retry.AgentRun.ID, "codex", threadID, "turn-recovered-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if next, err := store.DeliveryCandidate(ctx, job.ID); err != nil || next == nil || next.Message.ID != later.ID {
		t.Fatalf("next candidate=%#v err=%v, want later Follow", next, err)
	}
}

func TestFailedAcceptedTurnRequiresLaterSuccessfulFollowBeforeRevisionObservation(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "failed-observation-gate")
	first, err := store.NextDelivery(ctx, job.ID)
	if err != nil || first == nil {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	firstTurnID := "turn-failed-" + job.ID
	if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, firstTurnID, "failed"); err != nil {
		t.Fatal(err)
	}
	if run, ready, err := store.RevisionCandidate(ctx, job.ID, job.Revision); err != nil || ready || run.ID != "" {
		t.Fatalf("failed accepted turn crossed Revision observation gate: run=%#v ready=%v err=%v", run, ready, err)
	}
	follow, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "successful-follow", Input: "finish the coding workflow"})
	if err != nil || !created {
		t.Fatalf("follow=%#v created=%v err=%v", follow, created, err)
	}
	completed := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-success-"+job.ID)
	if run, ready, err := store.RevisionCandidate(ctx, job.ID, job.Revision); err != nil || !ready || run.ID != completed.ID {
		t.Fatalf("successful later follow did not become the Revision candidate: run=%#v ready=%v err=%v", run, ready, err)
	}
}

func prepareTransportIntegrationJob(t *testing.T, store postgres.Store, label string) (spine.Job, string) {
	t.Helper()
	ctx := context.Background()
	revision := strings.Repeat("a", 40)
	key := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: key, Goal: "transport proof", Repository: "https://github.com/aphronio/dorf.git", Revision: revision, Branch: "dorf/" + label, ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
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
	threadID := "thread-" + job.ID
	return job, threadID
}

func TestChangedAndUnchangedRevisionObservationsLinkExactImplementationAgentRuns(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	start, changed := strings.Repeat("1", 40), strings.Repeat("2", 40)
	input := postgres.NewJob{AdmissionKey: fmt.Sprintf("revision-evidence-%d", time.Now().UnixNano()), Goal: "bounded implementation", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/revision-evidence", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	now := time.Now().UTC().Round(time.Microsecond)
	setup, err := store.BeginSetup(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSetup(ctx, setup.ID, integrationEvidence(setup.ID, "repository-setup", setup.ID, "", start, "a"), spine.CommandObservation{Command: "prepare", StartedAt: now, FinishedAt: now}, []spine.DeclaredCheck{{Name: "check", Command: "go test ./..."}}); err != nil {
		t.Fatal(err)
	}
	threadID := "thread-" + job.ID
	changedRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-changed-"+job.ID)
	changedEvidence := integrationEvidence(changedRun.ID, "git-revision", "", "", changed, "b")
	changedEvidence.AgentRunID = changedRun.ID
	changedObservation := spine.RevisionObservation{ComparisonBase: start, Revision: changed, Tree: strings.Repeat("4", 40), Branch: input.Branch, StartedAt: now, FinishedAt: now}
	if recorded, err := store.RecordRevisionObservation(ctx, job.ID, changedRun.ID, changedObservation, changedEvidence); err != nil || !recorded {
		t.Fatalf("changed Revision observation recorded=%v err=%v", recorded, err)
	}

	failed, err := store.BeginCheck(ctx, job.ID, changed, "check", "go test ./...")
	if err != nil {
		t.Fatal(err)
	}
	failedEvidence := integrationEvidence(failed.ID, "check-output", "", failed.ID, changed, "c")
	if err := store.RecordCheck(ctx, failed, failedEvidence, spine.CommandObservation{Command: failed.Command, ExitCode: 1, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	failed.State, failed.ExitCode, failed.EvidenceID = "failed", 1, failedEvidence.ID
	checkMessage, created, err := store.AdmitCheckMessage(ctx, failed)
	if err != nil || !created || checkMessage.FromKind != spine.MessageFromWorkflow || checkMessage.FromID != failed.ID {
		t.Fatalf("failed Check Message=%#v created=%v err=%v", checkMessage, created, err)
	}
	unchangedRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-unchanged-"+job.ID)
	unchangedEvidence := integrationEvidence(unchangedRun.ID, "git-revision", "", "", changed, "d")
	unchangedEvidence.AgentRunID = unchangedRun.ID
	unchangedObservation := spine.RevisionObservation{ComparisonBase: changed, Revision: changed, Tree: strings.Repeat("4", 40), Branch: input.Branch, StartedAt: now, FinishedAt: now}
	if recorded, err := store.RecordRevisionObservation(ctx, job.ID, unchangedRun.ID, unchangedObservation, unchangedEvidence); err != nil || !recorded {
		t.Fatalf("unchanged Revision observation recorded=%v err=%v", recorded, err)
	}
	stored, err := store.Job(ctx, job.ID)
	records, evidenceErr := store.Evidence(ctx, job.ID)
	if err != nil || evidenceErr != nil || stored.Revision != changed || changedRun.InputRevision != start || unchangedRun.InputRevision != changed || !slices.ContainsFunc(records, func(record spine.Evidence) bool {
		return record.ID == changedEvidence.ID && record.AgentRunID == changedRun.ID && record.Revision == changed
	}) || !slices.ContainsFunc(records, func(record spine.Evidence) bool {
		return record.ID == unchangedEvidence.ID && record.AgentRunID == unchangedRun.ID && record.Revision == changed
	}) {
		t.Fatalf("stored Job=%#v changedRun=%#v unchangedRun=%#v Evidence=%#v err=%v evidenceErr=%v", stored, changedRun, unchangedRun, records, err, evidenceErr)
	}
}

func TestAtomicReviewPolicyPersistsNoReviewAndStableSelectedRuns(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	for _, test := range []struct {
		name  string
		paths []string
		roles []policy.Role
	}{
		{name: "explicit no review", paths: []string{"docs/review.md"}},
		{name: "mandatory selected role", paths: []string{"internal/auth/session.go"}, roles: []policy.Role{policy.RoleAuthAuthority}},
		{name: "multiple mandatory roles", paths: []string{"internal/auth/session.go", "web/login.tsx"}, roles: []policy.Role{policy.RoleAuthAuthority, policy.RoleBrowserUI}},
	} {
		t.Run(test.name, func(t *testing.T) {
			job, revision, _ := prepareReviewIntegrationJob(t, store, test.name)
			facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, test.paths, true, false)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := policy.ReviewPolicy(facts)
			if err != nil {
				t.Fatal(err)
			}
			record := spine.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}
			if err := store.RecordReviewPolicy(ctx, record); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordReviewPolicy(ctx, record); err != nil {
				t.Fatalf("idempotent policy retry: %v", err)
			}
			persisted, err := store.ReviewPlan(ctx, job.ID, revision)
			if err != nil || persisted.RecordedAt.IsZero() || persisted.Facts.Revision != revision || persisted.Plan.Decision != plan.Decision {
				t.Fatalf("persisted plan=%#v err=%v", persisted, err)
			}
			runs, err := store.ReviewRuns(ctx, job.ID, revision)
			if len(test.roles) == 0 {
				if err != nil || len(runs) != 0 || persisted.Plan.Decision != "no-review" {
					t.Fatalf("no-review runs=%#v plan=%#v err=%v", runs, persisted, err)
				}
			} else {
				if err != nil || len(runs) != len(test.roles) {
					t.Fatalf("selected runs=%#v err=%v", runs, err)
				}
				for i, role := range test.roles {
					request := runs[i].Request
					wantInput := policy.RolePrompt(role, facts, []string{"check"})
					if runs[i].Role != string(role) || runs[i].ID != spine.AgentRunID(request.ID) || runs[i].MessageID != request.ID || runs[i].Capability != spine.ReviewReadOnlyCapability || runs[i].InputRevision != revision || request.ID != spine.ReviewRequestMessageID(job.ID, revision, string(role)) || request.JobID != job.ID || request.FromKind != spine.MessageFromWorkflow || request.FromID != spine.ReviewRequestFromID(revision, string(role)) || request.Sequence != int64(i+2) || request.Input != wantInput || request.Intent != spine.MessageFollow || request.TargetTurnID != "" || request.AdmittedAt.IsZero() || runs[i].SandboxID != runs[i].Sandbox.ID || runs[i].Sandbox.ID != spine.ReviewSandboxName(runs[i].ID) || runs[i].Sandbox.JobID != job.ID || len(runs[i].Sandbox.OwnershipNonce) != 64 || len(runs[i].SubmissionNonce) != 64 {
						t.Fatalf("selected runs=%#v err=%v", runs, err)
					}
					for prior := 0; prior < i; prior++ {
						if runs[prior].Sandbox.ID == runs[i].Sandbox.ID || runs[prior].Sandbox.OwnershipNonce == runs[i].Sandbox.OwnershipNonce || runs[prior].SubmissionNonce == runs[i].SubmissionNonce {
							t.Fatalf("review Roles share an isolated resource identity: %#v", runs)
						}
					}
				}
			}
			changed := record
			changed.Plan.Decision = "invalid-retry-change"
			if err := store.RecordReviewPolicy(ctx, changed); err == nil || !strings.Contains(err.Error(), "changed across retry") {
				t.Fatalf("changed atomic policy error=%v", err)
			}
		})
	}
}

func TestReviewerFeedbackBecomesIdempotentObservedImplementationMessage(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, message, implementationRun := prepareReviewFeedbackIntegration(t, store, "review-feedback-message")

	if message.FromKind != "agent" || message.FromID == "" || message.ID != spine.MessageID(job.ID, "agent", message.FromID) || implementationRun.MessageID != message.ID || implementationRun.Role != "implement" || implementationRun.ID != spine.AgentRunID(message.ID) {
		t.Fatalf("review feedback Message=%#v implementation AgentRun=%#v", message, implementationRun)
	}
	evidence := integrationEvidence(implementationRun.ID, "git-revision", "", "", revision, "9")
	evidence.AgentRunID = implementationRun.ID
	observation := spine.RevisionObservation{ComparisonBase: revision, Revision: revision, Tree: strings.Repeat("c", 40), Branch: job.Branch}
	if recorded, err := store.RecordRevisionObservation(ctx, job.ID, implementationRun.ID, observation, evidence); err != nil || !recorded {
		t.Fatalf("unchanged review feedback observation recorded=%t err=%v", recorded, err)
	}
	stored, err := store.Job(ctx, job.ID)
	if err != nil || stored.Revision != revision {
		t.Fatalf("unchanged review feedback Job=%#v err=%v", stored, err)
	}
}

func TestReviewFeedbackReplaySurvivesClosureButNewFeedbackDoesNot(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, revision, _ := prepareReviewIntegrationJob(t, store, "review-feedback-closed-admission")
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"internal/auth/session.go", "web/login.tsx"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReviewPolicy(ctx, spine.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || len(runs) != 2 {
		t.Fatalf("review runs=%#v err=%v", runs, err)
	}
	feedback := func(run spine.ReviewRunView, digestByte string) (spine.HarnessTurn, spine.Evidence) {
		prepareReviewBoundaryIntegration(t, store, run)
		refreshed, err := store.ReviewRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		observed := integrationEvidence(refreshed.ID, "review-observation", "", "", revision, digestByte)
		observed.AgentRunID, observed.Producer = refreshed.ID, "dorf-agent-review"
		observed.StartedAt, observed.FinishedAt = refreshed.StartedAt, refreshed.FinishedAt
		return spine.HarnessTurn{ID: refreshed.TurnID, Status: "completed", Output: "bounded feedback from " + refreshed.Role}, observed
	}
	firstOutcome, firstEvidence := feedback(runs[0], "6")
	first, created, err := store.RecordReviewFeedback(ctx, runs[0].ID, firstOutcome, firstEvidence)
	if err != nil || !created {
		t.Fatalf("first review feedback=%#v created=%t err=%v", first, created, err)
	}
	secondOutcome, secondEvidence := feedback(runs[1], "5")
	if err := store.CloseAdmission(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	replayed, created, err := store.RecordReviewFeedback(ctx, runs[0].ID, firstOutcome, firstEvidence)
	if err != nil || created || replayed != first {
		t.Fatalf("closed-Job feedback replay=%#v created=%t err=%v", replayed, created, err)
	}
	if _, _, err := store.RecordReviewFeedback(ctx, runs[1].ID, secondOutcome, secondEvidence); err == nil || !strings.Contains(err.Error(), "cannot accept new review feedback") {
		t.Fatalf("new feedback after closure error=%v", err)
	}
}

func prepareReviewFeedbackIntegration(t *testing.T, store postgres.Store, suffix string) (spine.Job, string, spine.Message, spine.AgentRun) {
	t.Helper()
	ctx := context.Background()
	job, revision, _ := prepareReviewIntegrationJob(t, store, suffix)
	facts, err := policy.FactsFromPaths(strings.Repeat("a", 40), revision, []string{"internal/auth/session.go"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := policy.ReviewPolicy(facts)
	if err != nil {
		t.Fatal(err)
	}
	record := spine.ReviewPlanRecord{JobID: job.ID, Revision: revision, Facts: facts, Plan: plan}
	if err := store.RecordReviewPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	runs, err := store.ReviewRuns(ctx, job.ID, revision)
	if err != nil || len(runs) != 1 {
		t.Fatalf("review runs=%#v err=%v", runs, err)
	}
	reviewerRun := runs[0]
	request := reviewerRun.Request
	if reviewerRun.MessageID != request.ID || request.ID != spine.ReviewRequestMessageID(job.ID, revision, reviewerRun.Role) || request.FromID != spine.ReviewRequestFromID(revision, reviewerRun.Role) || request.Input == "" {
		t.Fatalf("review request Message -> AgentRun chain run=%#v request=%#v", reviewerRun.AgentRun, request)
	}
	prepareReviewBoundaryIntegration(t, store, reviewerRun)
	reviewerRun, err = store.ReviewRun(ctx, reviewerRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	feedback := "The authority path needs one bounded implementation adjustment."
	observed := integrationEvidence(reviewerRun.ID, "review-observation", "", "", revision, "7")
	observed.AgentRunID = reviewerRun.ID
	observed.Producer = "dorf-agent-review"
	observed.StartedAt, observed.FinishedAt = reviewerRun.StartedAt, reviewerRun.FinishedAt
	outcome := spine.HarnessTurn{ID: "turn-" + reviewerRun.ID, Status: "completed", Output: feedback}
	if err := store.SetWorkflowAttention(ctx, job.ID, reviewerRun.ID, "review feedback is not yet durable"); err != nil {
		t.Fatal(err)
	}
	message, created, err := store.RecordReviewFeedback(ctx, reviewerRun.ID, outcome, observed)
	if err != nil || !created || message.Input != feedback || message.FromKind != "agent" || message.FromID != reviewerRun.ID || message.AdmittedAt.IsZero() {
		t.Fatalf("review feedback Message=%#v created=%t err=%v", message, created, err)
	}
	cleared, err := store.Job(ctx, job.ID)
	if err != nil || cleared.WorkflowAttention != "" || cleared.WorkflowAttentionSource != "" {
		t.Fatalf("durable review feedback left stale attention: Job=%#v err=%v", cleared, err)
	}
	if err := store.SetWorkflowAttention(ctx, job.ID, reviewerRun.ID, "review feedback replay is not yet reconciled"); err != nil {
		t.Fatal(err)
	}
	repeated, created, err := store.RecordReviewFeedback(ctx, reviewerRun.ID, outcome, observed)
	if err != nil || created || repeated != message {
		t.Fatalf("idempotent review feedback Message=%#v created=%t err=%v", repeated, created, err)
	}
	cleared, err = store.Job(ctx, job.ID)
	if err != nil || cleared.WorkflowAttention != "" || cleared.WorkflowAttentionSource != "" {
		t.Fatalf("idempotent review feedback left stale attention: Job=%#v err=%v", cleared, err)
	}
	messages, err := store.Messages(ctx, job.ID)
	requestIndex := slices.IndexFunc(messages, func(candidate spine.MessageView) bool { return candidate.ID == request.ID })
	feedbackIndex := slices.IndexFunc(messages, func(candidate spine.MessageView) bool { return candidate.ID == message.ID })
	if err != nil || requestIndex < 0 || feedbackIndex < 0 || messages[requestIndex].AgentRunID != reviewerRun.ID || messages[requestIndex].State != spine.AgentRunCompleted || messages[feedbackIndex].AgentRunID != spine.AgentRunID(message.ID) || messages[feedbackIndex].State != spine.AgentRunPending {
		t.Fatalf("review request -> review AgentRun -> feedback Message chain=%#v err=%v", messages, err)
	}
	delivery, err := store.NextDelivery(ctx, job.ID)
	if err != nil || delivery == nil || delivery.Message != message || delivery.AgentRun.MessageID != message.ID || delivery.AgentRun.Role != "implement" {
		t.Fatalf("review feedback implementation delivery=%#v err=%v", delivery, err)
	}
	implementationRun := completeNextIntegrationRun(t, store, job.ID, "thread-"+job.ID, "turn-feedback-"+job.ID)
	if implementationRun.MessageID != message.ID || message.FromKind != spine.MessageFromAgent || message.FromID != reviewerRun.ID {
		t.Fatalf("feedback Message -> implementation AgentRun chain message=%#v run=%#v", message, implementationRun)
	}
	return job, revision, message, implementationRun
}

func TestSandboxCleanupRequiresRouteRevokeAndSuccessIsIdempotent(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, created, err := store.Admit(ctx, postgres.NewJob{
		AdmissionKey: "cleanup-order-" + fmt.Sprint(time.Now().UnixNano()),
		Goal:         "prove exact cleanup order", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("a", 40), Branch: "dorf/cleanup-order",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high",
		GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield",
	})
	if err != nil || !created {
		t.Fatalf("admit Job=%#v created=%t err=%v", job, created, err)
	}
	sandboxID := spine.MainSandboxName(job.ID)
	remove, err := store.GetOrCreateSandboxAction(ctx, sandboxID, spine.ActionSandboxDelete)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, remove.ID); err == nil {
		t.Fatal("Sandbox delete succeeded before its exact Route revoke Action")
	}
	stillUnsettled, err := store.GetOrCreateSandboxAction(ctx, sandboxID, spine.ActionSandboxDelete)
	if err != nil || stillUnsettled.ID != remove.ID || stillUnsettled.State != spine.ActionUnsettled {
		t.Fatalf("premature Sandbox delete=%#v err=%v", stillUnsettled, err)
	}

	revoke, err := store.GetOrCreateSandboxAction(ctx, sandboxID, spine.ActionRouteRevoke)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, revoke.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, revoke.ID); err != nil {
		t.Fatalf("idempotent Route revoke success: %v", err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, remove.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, remove.ID); err != nil {
		t.Fatalf("idempotent Sandbox delete success: %v", err)
	}
	retryRemove, err := store.GetOrCreateSandboxAction(ctx, sandboxID, spine.ActionSandboxDelete)
	if err != nil || retryRemove.ID != remove.ID || retryRemove.State != spine.ActionSucceeded {
		t.Fatalf("Sandbox cleanup retry=%#v err=%v", retryRemove, err)
	}
}

func prepareReviewBoundaryIntegration(t *testing.T, store postgres.Store, run spine.ReviewRunView) {
	t.Helper()
	ctx := context.Background()
	prepareReviewBoundaryResourcesIntegration(t, store, run)
	if err := store.PrepareAgentRun(ctx, run.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	threadID := "review-thread-" + run.ID
	if err := store.BindAgentRun(ctx, run.ID, "codex", threadID, "turn-"+run.ID, "completed"); err != nil {
		t.Fatal(err)
	}
}

func prepareReviewBoundaryResourcesIntegration(t *testing.T, store postgres.Store, run spine.ReviewRunView) {
	t.Helper()
	ctx := context.Background()
	sandbox, err := store.GetOrCreateSandboxAction(ctx, run.Sandbox.ID, spine.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.State != spine.ActionSucceeded {
		if err := store.RecordSandboxActionSuccess(ctx, sandbox.ID); err != nil {
			t.Fatal(err)
		}
	}
	checkout, err := store.GetOrCreateSandboxAction(ctx, run.Sandbox.ID, spine.ActionReviewCheckout)
	if err != nil {
		t.Fatal(err)
	}
	if checkout.State != spine.ActionSucceeded {
		if err := store.RecordSandboxActionSuccess(ctx, checkout.ID); err != nil {
			t.Fatal(err)
		}
	}
	route, err := store.GetOrCreateSandboxAction(ctx, run.Sandbox.ID, spine.ActionRouteCreate)
	if err != nil {
		t.Fatal(err)
	}
	if route.State != spine.ActionSucceeded {
		if err := store.RecordSandboxActionSuccess(ctx, route.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func prepareReviewIntegrationJob(t *testing.T, store postgres.Store, suffix string) (spine.Job, string, string) {
	t.Helper()
	ctx := context.Background()
	start, revision := strings.Repeat("a", 40), strings.Repeat("b", 40)
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: fmt.Sprintf("review-policy-%s-%d", strings.ReplaceAll(suffix, " ", "-"), time.Now().UnixNano()), Goal: "bounded implementation", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/review-policy", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
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
	implementationRun := completeNextIntegrationRun(t, store, job.ID, "thread-"+job.ID, "turn-"+job.ID)
	candidate, ready, err := store.RevisionCandidate(ctx, job.ID, start)
	if err != nil || !ready || candidate.ID != implementationRun.ID {
		t.Fatalf("Revision candidate=%#v ready=%v err=%v", candidate, ready, err)
	}
	evidence := integrationEvidence(candidate.ID, "git-revision", "", "", revision, "2")
	evidence.AgentRunID = candidate.ID
	if recorded, err := store.RecordRevisionObservation(ctx, job.ID, candidate.ID, spine.RevisionObservation{ComparisonBase: start, Revision: revision, Tree: strings.Repeat("c", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}, evidence); err != nil || !recorded {
		t.Fatalf("Revision recorded=%v err=%v", recorded, err)
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
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: fmt.Sprintf("setup-retry-%d", time.Now().UnixNano()), Goal: "bounded setup retry", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/setup-retry", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
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
	collidingPublic, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "setup-retry:toolchain-repair-1", Input: "ordinary caller input"})
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
	selected, selectedErr := store.SelectedSetup(ctx, job.ID)
	if err != nil || actionsErr != nil || selectedErr != nil || selected == nil || selected.ID != failed.ID || selected.State != spine.ActionFailed || job.WorkflowAttentionSource != failed.ID || invalidActionPresent {
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
	selected, selectedErr = store.SelectedSetup(ctx, job.ID)
	if err != nil || selectedErr != nil || selected == nil || selected.ID != secondRetry.ID || selected.State != spine.ActionSucceeded || job.WorkflowAttention != "" || job.WorkflowAttentionSource != "" {
		t.Fatalf("recovered Job=%#v selected setup=%#v err=%v selectedErr=%v", job, selected, err, selectedErr)
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

func TestRevisionObservationBoundaryIncludesLateSteeringAtomically(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	start := strings.Repeat("6", 40)
	revision := strings.Repeat("7", 40)
	key := fmt.Sprintf("revision-observation-boundary-%d", time.Now().UnixNano())
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: key, Goal: "bounded implementation", Repository: "https://github.com/aphronio/dorf.git", Revision: start, Branch: "dorf/revision-observation-boundary", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
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
	threadID := "thread-" + job.ID
	completeNextIntegrationRun(t, store, job.ID, threadID, "turn-initial-"+job.ID)
	if delivery, err := store.NextDelivery(ctx, job.ID); err != nil || delivery != nil {
		t.Fatalf("pre-boundary delivery=%#v err=%v", delivery, err)
	}

	late, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "late-before-observation", Input: "include this bounded steering"})
	if err != nil || !created {
		t.Fatalf("late admission=%#v created=%v err=%v", late, created, err)
	}
	if run, ready, err := store.RevisionCandidate(ctx, job.ID, start); err != nil || ready || run.ID != "" {
		t.Fatalf("Revision candidate crossed admitted FIFO: run=%#v ready=%v err=%v", run, ready, err)
	}
	includedRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-late-"+job.ID)
	candidate, ready, err := store.RevisionCandidate(ctx, job.ID, start)
	if err != nil || !ready || candidate.ID != includedRun.ID {
		t.Fatalf("Revision candidate=%#v ready=%v err=%v", candidate, ready, err)
	}
	observation := spine.RevisionObservation{ComparisonBase: start, Revision: revision, Tree: strings.Repeat("8", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}
	evidence := integrationEvidence(candidate.ID, "git-revision", "", "", revision, "a")
	evidence.AgentRunID = candidate.ID
	afterCandidate, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "late-after-candidate", Input: "include the message admitted during Git inspection"})
	if err != nil || !created {
		t.Fatalf("late post-candidate admission=%#v created=%v err=%v", afterCandidate, created, err)
	}
	if recorded, err := store.RecordRevisionObservation(ctx, job.ID, candidate.ID, observation, evidence); err != nil || recorded {
		t.Fatalf("Revision observation skipped late accepted input: recorded=%v err=%v", recorded, err)
	}
	finalRun := completeNextIntegrationRun(t, store, job.ID, threadID, "turn-after-candidate-"+job.ID)
	finalCandidate, ready, err := store.RevisionCandidate(ctx, job.ID, start)
	if err != nil || !ready || finalCandidate.ID != finalRun.ID {
		t.Fatalf("final Revision candidate=%#v ready=%v err=%v", finalCandidate, ready, err)
	}
	finalEvidence := integrationEvidence(finalCandidate.ID, "git-revision", "", "", revision, "b")
	finalEvidence.AgentRunID = finalCandidate.ID
	if recorded, err := store.RecordRevisionObservation(ctx, job.ID, finalCandidate.ID, observation, finalEvidence); err != nil || !recorded {
		t.Fatalf("final Revision observation recorded=%v err=%v", recorded, err)
	}
	retry, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: late.FromKind, FromID: late.FromID, Input: late.Input})
	if err != nil || created || retry != late {
		t.Fatalf("idempotent admitted retry=%#v created=%v err=%v", retry, created, err)
	}
}

func integrationEvidence(owner, kind, actionID, checkID, revision, digestByte string) spine.Evidence {
	now := time.Now().UTC().Round(time.Microsecond)
	return spine.Evidence{ID: spine.EvidenceID(owner, kind), Digest: strings.Repeat(digestByte, 64), ByteSize: 10, MediaType: "application/vnd.dorf.observation+json", Producer: "integration-test", Kind: kind, ActionID: actionID, CheckID: checkID, Revision: revision, StartedAt: now, FinishedAt: now}
}

func completeNextIntegrationRun(t *testing.T, store postgres.Store, jobID, threadID, turnID string) spine.AgentRun {
	t.Helper()
	delivery, err := store.NextDelivery(context.Background(), jobID)
	if err != nil || delivery == nil {
		t.Fatalf("next delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(context.Background(), delivery.AgentRun.ID, "codex", delivery.AgentRun.BaselineTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(context.Background(), delivery.AgentRun.ID, "codex", threadID, turnID, "completed"); err != nil {
		t.Fatal(err)
	}
	return delivery.AgentRun
}

func TestCleanupRecoversCompletedHarnessTurnAfterRunTaskExhaustion(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	job, created, err := workflow.Admit(ctx, store, client, providerCheck{}, postgres.NewJob{AdmissionKey: "cleanup-exhausted-" + suffix, Goal: "initial", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/cleanup-exhausted", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"})
	if err != nil || !created {
		t.Fatalf("admit Job created=%v err=%v", created, err)
	}
	sandbox, err := store.GetOrCreateSandboxAction(ctx, spine.MainSandboxName(job.ID), spine.ActionSandboxCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, sandbox.ID); err != nil {
		t.Fatal(err)
	}
	route, err := store.GetOrCreateSandboxAction(ctx, spine.MainSandboxName(job.ID), spine.ActionRouteCreate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSandboxActionSuccess(ctx, route.ID); err != nil {
		t.Fatal(err)
	}
	taskIDs := []string{job.TaskID}
	t.Cleanup(func() {
		for _, id := range taskIDs {
			_ = client.CancelTask(context.Background(), config.QueueName, id)
		}
	})
	threadID := "cleanup-thread-" + suffix
	delivery, err := store.NextDelivery(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	turnID := "cleanup-turn-" + suffix
	if err := store.BindAgentRun(ctx, delivery.AgentRun.ID, "codex", threadID, turnID, "running"); err != nil {
		t.Fatal(err)
	}
	second, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, FromKind: "human", FromID: "later-pending", Input: "must not be submitted by cleanup"})
	if err != nil || !created || second.Sequence != 2 {
		t.Fatalf("later message=%#v created=%v err=%v", second, created, err)
	}
	externals := &integrationExternals{turns: []spine.HarnessTurn{{ID: turnID, Status: "completed"}}, submitted: []int64{1}}
	cleaning, err := workflow.ScheduleCleanup(ctx, store, client, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskIDs = append(taskIDs, cleaning.CleanupTaskID)
	if cleaning.AdmissionOpen {
		t.Fatalf("cleanup did not close admission: %#v", cleaning)
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
	if cleaned.CleanupState != spine.CleanupComplete {
		t.Fatalf("cleaned Job=%#v", cleaned)
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].State != spine.AgentRunCompleted || messages[0].TurnID != turnID || messages[1].State != spine.AgentRunInterrupted || messages[1].TurnID != "" {
		t.Fatalf("cleanup delivery truth=%#v", messages)
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[1]" {
		t.Fatalf("cleanup submitted pending FIFO input: %v", got)
	}
	if got := externals.effectKinds(); fmt.Sprint(got) != "[provider-route-revoke sandbox-delete]" {
		t.Fatalf("cleanup effects=%v", got)
	}
	snapshot, err := client.FetchTaskResult(ctx, config.QueueName, job.TaskID)
	if err != nil || snapshot == nil || snapshot.State != absurd.TaskCancelled {
		t.Fatalf("cancelled public run result=%#v err=%v", snapshot, err)
	}
}

type integrationExternals struct {
	mu        sync.Mutex
	turns     []spine.HarnessTurn
	submitted []int64
	effects   []spine.ActionKind
}

func (*integrationExternals) Harness() string { return "codex" }

func (e *integrationExternals) effect(kind spine.ActionKind) error {
	e.mu.Lock()
	e.effects = append(e.effects, kind)
	e.mu.Unlock()
	return nil
}
func (e *integrationExternals) SandboxCreate(context.Context, spine.Job, spine.Sandbox) error {
	return e.effect(spine.ActionSandboxCreate)
}
func (e *integrationExternals) RepositoryClone(context.Context, spine.Job, spine.Sandbox) error {
	return e.effect(spine.ActionRepositoryClone)
}
func (e *integrationExternals) RouteCreate(context.Context, spine.Job, spine.Sandbox, spine.Route) error {
	return e.effect(spine.ActionRouteCreate)
}
func (e *integrationExternals) AgentInitialTurn(_ context.Context, job spine.Job, delivery spine.Delivery) (spine.HarnessBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.turns) == 0 {
		turn := spine.HarnessTurn{ID: "integration-turn-" + delivery.Message.ID, Status: "running"}
		e.submitted = append(e.submitted, delivery.Message.Sequence)
		e.turns = append(e.turns, turn)
	}
	return spine.HarnessBinding{Harness: "codex", ThreadID: "integration-thread-" + job.ID, Turn: e.turns[0]}, nil
}
func (e *integrationExternals) AgentInitialTurns(_ context.Context, job spine.Job) (spine.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return spine.HarnessHistory{Harness: "codex", ThreadID: "integration-thread-" + job.ID, Turns: append([]spine.HarnessTurn(nil), e.turns...)}, nil
}
func (e *integrationExternals) AgentTurns(_ context.Context, _ spine.Job, threadID string) (spine.HarnessHistory, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return spine.HarnessHistory{Harness: "codex", ThreadID: threadID, Turns: append([]spine.HarnessTurn(nil), e.turns...)}, nil
}
func (e *integrationExternals) AgentSubmit(_ context.Context, _ spine.Job, delivery spine.Delivery) (spine.HarnessBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	turn := spine.HarnessTurn{ID: "integration-turn-" + delivery.Message.ID, Status: "running"}
	e.submitted = append(e.submitted, delivery.Message.Sequence)
	e.turns = append(e.turns, turn)
	return spine.HarnessBinding{Harness: "codex", ThreadID: delivery.AgentRun.ThreadID, Turn: turn}, nil
}
func (e *integrationExternals) AgentSteer(_ context.Context, _ spine.Job, delivery spine.Delivery) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.submitted = append(e.submitted, delivery.Message.Sequence)
	return delivery.Message.TargetTurnID, nil
}
func (e *integrationExternals) AgentWait(_ context.Context, _ spine.Job, threadID, turnID string) (spine.HarnessBinding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for index := range e.turns {
		if e.turns[index].ID == turnID {
			e.turns[index].Status = "completed"
		}
	}
	return spine.HarnessBinding{Harness: "codex", ThreadID: threadID, Turn: spine.HarnessTurn{ID: turnID, Status: "completed"}}, nil
}
func (e *integrationExternals) RouteRevoke(context.Context, spine.Job, spine.Sandbox, spine.Route) error {
	return e.effect(spine.ActionRouteRevoke)
}
func (e *integrationExternals) SandboxDelete(context.Context, spine.Job, spine.Sandbox) error {
	return e.effect(spine.ActionSandboxDelete)
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

func TestMessageTaskAttachmentCompareAndSetRejectsAnotherStoredTask(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	input := postgres.NewJob{AdmissionKey: "reattach-cas-" + suffix, Goal: "preserve one task binding", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/reattach-cas", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high", GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "greenfield"}
	job, created, err := store.Admit(ctx, input)
	if err != nil || !created {
		t.Fatalf("admit created=%v err=%v", created, err)
	}
	unrelated, err := client.Spawn(ctx, "unrelated-task", workflow.Params{JobID: job.ID}, absurd.SpawnOptions{QueueName: config.QueueName, IdempotencyKey: "unrelated:" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, unrelated.TaskID) })
	if err := store.AttachMessageTask(ctx, job.ID, unrelated.TaskID); err != nil {
		t.Fatal(err)
	}
	spawned, err := client.Spawn(ctx, postgres.MessageTaskName, workflow.Params{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: postgres.MessageTaskKey(job.ID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.CancelTask(context.Background(), config.QueueName, spawned.TaskID) })
	if err := store.AttachMessageTask(ctx, job.ID, spawned.TaskID); err == nil {
		t.Fatal("a second public Spawn result replaced the stored task binding")
	}
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

func TestClosedAdmissionRejectsLateCheckFeedbackDeliveryAndRevisionObservation(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	job, _ := prepareTransportIntegrationJob(t, store, "closed-check-feedback")
	check, err := store.BeginCheck(ctx, job.ID, job.Revision, "check", "go test ./focused")
	if err != nil {
		t.Fatal(err)
	}
	evidence := integrationEvidence(check.ID, "check-output", "", check.ID, job.Revision, "1")
	if err := store.RecordCheck(ctx, check, evidence, spine.CommandObservation{Command: check.Command, ExitCode: 1, StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatal(err)
	}
	check.State, check.ExitCode, check.EvidenceID = "failed", 1, evidence.ID
	if err := store.CloseAdmission(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, created, err := store.AdmitCheckMessage(ctx, check); err == nil || created {
		t.Fatalf("closed admission accepted failed-Check feedback: created=%t err=%v", created, err)
	}
	if delivery, err := store.NextDelivery(ctx, job.ID); err != nil || delivery != nil {
		t.Fatalf("closed admission selected harness delivery: delivery=%#v err=%v", delivery, err)
	}
	messages, err := store.Messages(ctx, job.ID)
	if err != nil || len(messages) != 1 {
		t.Fatalf("closed admission retained unexpected Message: messages=%#v err=%v", messages, err)
	}

	observedJob, threadID := prepareTransportIntegrationJob(t, store, "closed-revision-observation")
	run := completeNextIntegrationRun(t, store, observedJob.ID, threadID, "turn-closed-observation")
	observation := spine.RevisionObservation{
		ComparisonBase: observedJob.Revision, Revision: strings.Repeat("9", 40), Tree: strings.Repeat("8", 40),
		Branch: observedJob.Branch, StartedAt: now, FinishedAt: now,
	}
	observedEvidence := integrationEvidence(run.ID, "git-revision", "", "", observation.Revision, "2")
	observedEvidence.AgentRunID = run.ID
	if err := store.CloseAdmission(ctx, observedJob.ID); err != nil {
		t.Fatal(err)
	}
	if recorded, err := store.RecordRevisionObservation(ctx, observedJob.ID, run.ID, observation, observedEvidence); err != nil || recorded {
		t.Fatalf("closed admission recorded new Revision observation: recorded=%t err=%v", recorded, err)
	}
}

func TestCleanupCompletesWithExplanatoryWorkflowAttention(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _ := prepareTransportIntegrationJob(t, store, "cleanup-attention")
	if err := store.SetWorkflowAttention(ctx, job.ID, "operator:test", "explanatory only"); err != nil {
		t.Fatal(err)
	}
	runs, err := store.AgentRuns(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if err := store.InterruptAgentRun(ctx, run.ID, "cleanup"); err != nil {
			t.Fatal(err)
		}
	}
	sandboxID := spine.MainSandboxName(job.ID)
	for _, kind := range []spine.ActionKind{spine.ActionRouteRevoke, spine.ActionSandboxDelete} {
		action, err := store.GetOrCreateSandboxAction(ctx, sandboxID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CloseAdmission(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCleanupTaskID(ctx, job.ID, "cleanup-task-"+job.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != spine.CleanupComplete || cleaned.WorkflowAttention != "" || cleaned.WorkflowAttentionSource != "" || !cleaned.WorkflowAttentionAt.IsZero() {
		t.Fatalf("cleanup terminal retained explanatory attention: job=%#v err=%v", cleaned, err)
	}
}
