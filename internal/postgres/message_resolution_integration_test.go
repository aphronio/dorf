package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/aphronio/dorf/internal/workflow"
)

func resolutionJob(t *testing.T, store postgres.Store, suffix string) (spine.Job, string) {
	t.Helper()
	ctx := context.Background()
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: "resolution-" + suffix + fmt.Sprint(time.Now().UnixNano()), Goal: "initial", Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("1", 40), Branch: "dorf/resolution", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high"})
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	if _, err := store.DB.ExecContext(ctx, `update dorf.jobs set workflow_phase='implementing' where id=$1`, job.ID); err != nil {
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

func failNoSubmit(t *testing.T, store postgres.Store, jobID, sessionID string) spine.Delivery {
	t.Helper()
	ctx := context.Background()
	delivery, err := store.NextDelivery(ctx, jobID, sessionID)
	if err != nil || delivery == nil {
		t.Fatalf("delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailAgentRun(ctx, delivery.AgentRun.ID, "native history positively proved no turn was submitted"); err != nil {
		t.Fatal(err)
	}
	return *delivery
}

func failNativeTurn(t *testing.T, store postgres.Store, jobID, sessionID string) spine.Delivery {
	t.Helper()
	ctx := context.Background()
	delivery, err := store.NextDelivery(ctx, jobID, sessionID)
	if err != nil || delivery == nil {
		t.Fatalf("delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, delivery.AgentRun.ID, "failed-native-"+delivery.Message.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	return *delivery
}

func TestMessageResolutionDiagnosisIdempotencyCanonicalSettlementAndWakeHighWater(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	job, sessionID := resolutionJob(t, store, "canonical-")
	failed := failNativeTurn(t, store, job.ID, sessionID)
	second, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "second", Input: "continue after explicit loss"})
	if err != nil || !created || second.Sequence != 2 {
		t.Fatalf("second=%#v created=%v err=%v", second, created, err)
	}
	if action, started, err := store.BeginCommit(ctx, job.ID, job.Revision); err != nil || started || action.ID != "" {
		t.Fatalf("commit crossed unresolved input action=%#v started=%v err=%v", action, started, err)
	}

	beforeCount := 0
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.message_resolutions where job_id=$1`, job.ID).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	diagnosis, proposed, err := store.PlanMessageResolution(ctx, postgres.MessageResolutionInput{JobID: job.ID, MessageID: failed.Message.ID, Decision: spine.ResolutionAcknowledgeLoss, Authority: "owner", Reason: "accept the retained terminal native failure\n"})
	if err != nil || diagnosis.Message.ID != failed.Message.ID || diagnosis.AgentRun.ID != failed.AgentRun.ID || diagnosis.Action.ID != failed.AgentRun.ActionID || diagnosis.NativeSessionID != sessionID || diagnosis.NativeTurnID == "" || diagnosis.NativeOutcome != "failed" || proposed.ID != spine.MessageResolutionID(job.ID, failed.Message.ID) {
		t.Fatalf("dry-run diagnosis=%#v proposed=%#v err=%v", diagnosis, proposed, err)
	}
	var afterDryRun int
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.message_resolutions where job_id=$1`, job.ID).Scan(&afterDryRun); err != nil || afterDryRun != beforeCount {
		t.Fatalf("dry-run mutated receipts count=%d err=%v", afterDryRun, err)
	}

	input := postgres.MessageResolutionInput{JobID: job.ID, MessageID: failed.Message.ID, Decision: spine.ResolutionAcknowledgeLoss, Authority: "owner", Reason: "accept the retained terminal native failure\n"}
	receipt, created, err := workflow.ResolveMessage(ctx, store, client, input)
	if err != nil || !created || receipt.ReservedWakeSequence != 3 {
		t.Fatalf("receipt=%#v created=%v err=%v", receipt, created, err)
	}
	repeated, created, err := workflow.ResolveMessage(ctx, store, client, input)
	if err != nil || created || repeated != receipt {
		t.Fatalf("idempotent receipt=%#v created=%v err=%v", repeated, created, err)
	}
	var wakeRows int
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.e_dorf_jobs where event_name=$1`, workflow.WakeEvent(job.ID, 3)).Scan(&wakeRows); err != nil || wakeRows != 1 {
		t.Fatalf("idempotent receipt produced wake rows=%d err=%v", wakeRows, err)
	}
	changed := input
	changed.Reason = "changed bytes\n"
	if _, _, err := store.ResolveMessage(ctx, changed); err == nil || !strings.Contains(err.Error(), "conflicting immutable") {
		t.Fatalf("changed receipt error=%v", err)
	}
	changed = input
	changed.Authority = "another owner"
	if _, _, err := store.ResolveMessage(ctx, changed); err == nil || !strings.Contains(err.Error(), "conflicting immutable") {
		t.Fatalf("changed authority error=%v", err)
	}
	changed = input
	changed.Decision = spine.ResolutionAbandon
	if _, _, err := store.ResolveMessage(ctx, changed); err == nil || !strings.Contains(err.Error(), "conflicting immutable") {
		t.Fatalf("changed decision error=%v", err)
	}
	var state, nativeTurn, nativeOutcome, attention string
	if err := db.QueryRowContext(ctx, `select state,coalesce(native_turn_id,''),coalesce(native_outcome,''),coalesce(attention,'') from dorf.agent_runs where id=$1`, failed.AgentRun.ID).Scan(&state, &nativeTurn, &nativeOutcome, &attention); err != nil || state != "failed" || nativeTurn == "" || nativeOutcome != "failed" {
		t.Fatalf("acknowledgement rewrote failed run state=%s turn=%s outcome=%s attention=%s err=%v", state, nativeTurn, nativeOutcome, attention, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.message_resolutions set authority='rewritten' where id=$1`, receipt.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("receipt update was not rejected: %v", err)
	}
	if _, err := db.ExecContext(ctx, `delete from dorf.message_resolutions where id=$1`, receipt.ID); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("receipt delete was not rejected: %v", err)
	}

	views, err := store.Messages(ctx, job.ID)
	if err != nil || views[1].BlockingSeq != 0 || views[0].Resolution == nil {
		t.Fatalf("canonical status views=%#v err=%v", views, err)
	}
	if action, started, err := store.BeginCommit(ctx, job.ID, job.Revision); err != nil || started || action.ID != "" {
		t.Fatalf("commit crossed pending continuation action=%#v started=%v err=%v", action, started, err)
	}
	next, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || next == nil || next.Message.ID != second.ID {
		t.Fatalf("acknowledge-loss FIFO continuation=%#v err=%v", next, err)
	}
	if err := store.PrepareAgentRun(ctx, next.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, next.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	secondTurnID := "turn-second-" + job.ID
	if err := store.BindNativeTurn(ctx, next.AgentRun.ID, secondTurnID, "completed"); err != nil {
		t.Fatal(err)
	}
	fourth, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "fourth", Input: "must use high-water four"})
	if err != nil || !created || fourth.Sequence != 4 {
		t.Fatalf("post-resolution admission=%#v created=%v err=%v", fourth, created, err)
	}
	wake, err := store.NextWakeSequence(ctx, job.ID)
	if err != nil || wake != 4 {
		t.Fatalf("replayed wake 3 selected next wake=%d err=%v", wake, err)
	}
	fourthDelivery, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || fourthDelivery == nil || fourthDelivery.Message.ID != fourth.ID {
		t.Fatalf("sequence four delivery=%#v err=%v", fourthDelivery, err)
	}
	if err := store.PrepareAgentRun(ctx, fourthDelivery.AgentRun.ID, secondTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, fourthDelivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, fourthDelivery.AgentRun.ID, "turn-fourth-"+job.ID, "completed"); err != nil {
		t.Fatal(err)
	}
	if action, started, err := store.BeginCommit(ctx, job.ID, job.Revision); err != nil || !started || action.ID == "" {
		t.Fatalf("canonical settlement did not release commit action=%#v started=%v err=%v", action, started, err)
	}
}

type resolutionTestBarrier struct {
	point string
	err   error
}

func (b resolutionTestBarrier) ReachResolution(_ context.Context, point string, _ spine.MessageResolution) error {
	if point == b.point {
		return b.err
	}
	return nil
}

type abandonmentRaceBarrier struct {
	selected chan spine.Delivery
	release  chan struct{}
}

func (b abandonmentRaceBarrier) Reach(ctx context.Context, point string, delivery spine.Delivery) error {
	if point != spine.BarrierBeforeSubmit {
		return nil
	}
	select {
	case b.selected <- delivery:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type provenNoSubmitError struct{}

func (provenNoSubmitError) Error() string          { return "injected positive no-submit proof" }
func (provenNoSubmitError) DefiniteNoSubmit() bool { return true }

type abandonmentRaceExternals struct {
	*integrationExternals
}

func (e *abandonmentRaceExternals) AgentSubmit(_ context.Context, _ spine.Job, delivery spine.Delivery) (spine.NativeTurn, error) {
	e.mu.Lock()
	e.submitted = append(e.submitted, delivery.Message.Sequence)
	e.mu.Unlock()
	return spine.NativeTurn{}, provenNoSubmitError{}
}

func TestMessageAbandonmentSharesNativeMutationFence(t *testing.T) {
	db, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _ := resolutionJob(t, store, "abandonment-fence-")
	barrier := abandonmentRaceBarrier{selected: make(chan spine.Delivery, 1), release: make(chan struct{})}
	externals := &abandonmentRaceExternals{integrationExternals: &integrationExternals{}}
	service := spine.Service{Store: store, Externals: externals, Barrier: barrier}
	type runResult struct {
		disposition spine.RunDisposition
		err         error
	}
	runDone := make(chan runResult, 1)
	go func() {
		disposition, err := service.RunUntilIdle(ctx, job.ID)
		runDone <- runResult{disposition: disposition, err: err}
	}()

	var selected spine.Delivery
	select {
	case selected = <-barrier.selected:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not select the pending delivery before submission")
	}
	input := postgres.MessageResolutionInput{JobID: job.ID, MessageID: selected.Message.ID, Decision: spine.ResolutionAbandon, Authority: "operator", Reason: "authoritative abandonment must serialize with native mutation"}
	type resolutionResult struct {
		receipt spine.MessageResolution
		created bool
		err     error
	}
	resolutionDone := make(chan resolutionResult, 1)
	go func() {
		receipt, created, err := store.ResolveMessage(ctx, input)
		resolutionDone <- resolutionResult{receipt: receipt, created: created, err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := db.QueryRowContext(ctx, `select count(*) from pg_stat_activity where datname=current_database() and pid<>pg_backend_pid() and wait_event_type='Lock' and query like '%pg_advisory_xact_lock%'`).Scan(&waiting); err != nil {
			close(barrier.release)
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			close(barrier.release)
			t.Fatal("abandonment did not reach the held Job execution fence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case result := <-resolutionDone:
		close(barrier.release)
		t.Fatalf("abandonment crossed selected native delivery: %#v", result)
	default:
	}
	var receipts int
	var admissionOpen bool
	if err := db.QueryRowContext(ctx, `select admission_open,(select count(*) from dorf.message_resolutions where job_id=$1) from dorf.jobs where id=$1`, job.ID).Scan(&admissionOpen, &receipts); err != nil || !admissionOpen || receipts != 0 {
		close(barrier.release)
		t.Fatalf("blocked abandonment mutated state admission_open=%t receipts=%d err=%v", admissionOpen, receipts, err)
	}

	close(barrier.release)
	select {
	case result := <-runDone:
		if result.err != nil || result.disposition != spine.RunBlocked {
			t.Fatalf("worker reconciliation=%#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not release the Job execution fence")
	}
	var resolved resolutionResult
	select {
	case resolved = <-resolutionDone:
	case <-time.After(5 * time.Second):
		t.Fatal("abandonment did not resume after native reconciliation")
	}
	if resolved.err != nil || !resolved.created || resolved.receipt.Decision != spine.ResolutionAbandon {
		t.Fatalf("serialized abandonment=%#v", resolved)
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[1]" {
		t.Fatalf("native submissions before authoritative abandonment=%v", got)
	}
	if disposition, err := service.RunUntilIdle(ctx, job.ID); err != nil || disposition != spine.RunClosed {
		t.Fatalf("post-abandonment worker disposition=%s err=%v", disposition, err)
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[1]" {
		t.Fatalf("native submission occurred after authoritative abandonment: %v", got)
	}
	var runState string
	if err := db.QueryRowContext(ctx, `select j.admission_open,ar.state,(select count(*) from dorf.message_resolutions where job_id=j.id) from dorf.jobs j join dorf.agent_runs ar on ar.job_id=j.id where j.id=$1`, job.ID).Scan(&admissionOpen, &runState, &receipts); err != nil || admissionOpen || runState != string(spine.AgentRunFailed) || receipts != 1 {
		t.Fatalf("settled abandonment admission_open=%t run_state=%s receipts=%d err=%v", admissionOpen, runState, receipts, err)
	}
}

func TestMessageResolutionReceiptAndWakeFaultBarriersConverge(t *testing.T) {
	db, store, client := testDatabase(t)
	ctx := context.Background()
	job, sessionID := resolutionJob(t, store, "faults-")
	failed := failNoSubmit(t, store, job.ID, sessionID)
	input := postgres.MessageResolutionInput{JobID: job.ID, MessageID: failed.Message.ID, Decision: spine.ResolutionRetry, Authority: "operator", Reason: "positive no-submit proof"}
	barrierErr := fmt.Errorf("injected process loss")

	if _, _, err := workflow.ResolveMessage(ctx, store, client, input, resolutionTestBarrier{point: workflow.BarrierBeforeResolutionReceipt, err: barrierErr}); err == nil {
		t.Fatal("before-receipt barrier did not stop resolution")
	}
	var receipts, wakes int
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.message_resolutions where job_id=$1`, job.ID).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("before-receipt receipts=%d err=%v", receipts, err)
	}
	var retryMessages int
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.job_messages where retry_of_message_id=$1`, failed.Message.ID).Scan(&retryMessages); err != nil || retryMessages != 0 {
		t.Fatalf("before-receipt retry messages=%d err=%v", retryMessages, err)
	}
	if receipt, created, err := workflow.ResolveMessage(ctx, store, client, input, resolutionTestBarrier{point: workflow.BarrierAfterResolutionReceipt, err: barrierErr}); err == nil || !created || receipt.ReservedWakeSequence != 2 {
		t.Fatalf("after-receipt result=%#v created=%v err=%v", receipt, created, err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.message_resolutions where job_id=$1`, job.ID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("after-receipt receipts=%d err=%v", receipts, err)
	}
	var retryMessageID string
	if err := db.QueryRowContext(ctx, `select id from dorf.job_messages where retry_of_message_id=$1`, failed.Message.ID).Scan(&retryMessageID); err != nil || retryMessageID == "" {
		t.Fatalf("after-receipt retry message=%q err=%v", retryMessageID, err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.e_dorf_jobs where event_name=$1`, workflow.WakeEvent(job.ID, 2)).Scan(&wakes); err != nil || wakes != 0 {
		t.Fatalf("after-receipt wakes=%d err=%v", wakes, err)
	}
	if receipt, created, err := workflow.ResolveMessage(ctx, store, client, input, resolutionTestBarrier{point: workflow.BarrierAfterResolutionWake, err: barrierErr}); err == nil || created || receipt.ReservedWakeSequence != 2 || receipt.RetryMessageID != retryMessageID {
		t.Fatalf("after-wake result=%#v created=%v err=%v", receipt, created, err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.e_dorf_jobs where event_name=$1`, workflow.WakeEvent(job.ID, 2)).Scan(&wakes); err != nil || wakes != 1 {
		t.Fatalf("after-wake wakes=%d err=%v", wakes, err)
	}
	receipt, created, err := workflow.ResolveMessage(ctx, store, client, input)
	if err != nil || created || receipt.ReservedWakeSequence != 2 || receipt.RetryMessageID != retryMessageID {
		t.Fatalf("converged retry=%#v created=%v err=%v", receipt, created, err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from dorf.job_messages where retry_of_message_id=$1`, failed.Message.ID).Scan(&retryMessages); err != nil || retryMessages != 1 {
		t.Fatalf("converged retry messages=%d err=%v", retryMessages, err)
	}
	if err := db.QueryRowContext(ctx, `select count(*) from absurd.e_dorf_jobs where event_name=$1`, workflow.WakeEvent(job.ID, 2)).Scan(&wakes); err != nil || wakes != 1 {
		t.Fatalf("converged wake count=%d err=%v", wakes, err)
	}
}

func TestMessageResolutionRetrySafetyAndAbandonment(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()

	retryJob, retrySession := resolutionJob(t, store, "retry-")
	failed := failNoSubmit(t, store, retryJob.ID, retrySession)
	retryInput := postgres.MessageResolutionInput{JobID: retryJob.ID, MessageID: failed.Message.ID, Decision: spine.ResolutionRetry, Authority: "operator", Reason: "native history proves exact non-delivery"}
	receipt, created, err := store.ResolveMessage(ctx, retryInput)
	if err != nil || !created || receipt.ReservedWakeSequence != 2 || receipt.RetryMessageID == "" {
		t.Fatalf("retry receipt=%#v created=%v err=%v", receipt, created, err)
	}
	delivery, err := store.NextDelivery(ctx, retryJob.ID, retrySession)
	if err != nil || delivery == nil || delivery.Message.ID != receipt.RetryMessageID || delivery.Message.RetryOfMessageID != failed.Message.ID || delivery.Message.Input != failed.Message.Input || delivery.AgentRun.ID == failed.AgentRun.ID || delivery.AgentRun.ActionID == failed.AgentRun.ActionID || delivery.AgentRun.State != spine.AgentRunPending {
		t.Fatalf("authorized new-identity retry=%#v err=%v", delivery, err)
	}
	externals := &integrationExternals{}
	service := spine.Service{Store: store, Externals: externals}
	if disposition, err := service.RunUntilIdle(ctx, retryJob.ID); err != nil || disposition != spine.RunIdle {
		t.Fatalf("retry run disposition=%s err=%v", disposition, err)
	}
	if disposition, err := service.RunUntilIdle(ctx, retryJob.ID); err != nil || disposition != spine.RunIdle {
		t.Fatalf("retry replay disposition=%s err=%v", disposition, err)
	}
	if got := externals.submittedSequences(); fmt.Sprint(got) != "[2]" {
		t.Fatalf("authorized retry submitted native turns=%v", got)
	}

	failedJob, failedSession := resolutionJob(t, store, "native-failed-")
	nativeFailed, err := store.NextDelivery(ctx, failedJob.ID, failedSession)
	if err != nil || nativeFailed == nil {
		t.Fatal(err)
	}
	if err := store.PrepareAgentRun(ctx, nativeFailed.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, nativeFailed.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, nativeFailed.AgentRun.ID, "failed-native-turn-"+failedJob.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	unsafe := postgres.MessageResolutionInput{JobID: failedJob.ID, MessageID: nativeFailed.Message.ID, Decision: spine.ResolutionRetry, Authority: "operator", Reason: "blind retry request"}
	if _, _, err := store.ResolveMessage(ctx, unsafe); err == nil || !strings.Contains(err.Error(), "not proven safe") {
		t.Fatalf("native failure retry error=%v", err)
	}
	if err := store.UncertainAgentRun(ctx, nativeFailed.AgentRun.ID, "native history is ambiguous"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ResolveMessage(ctx, unsafe); err == nil || !strings.Contains(err.Error(), "not proven safe") {
		t.Fatalf("ambiguous retry error=%v", err)
	}
	abandon := unsafe
	abandon.Decision, abandon.Reason = spine.ResolutionAbandon, "owner abandons without cleanup"
	abandoned, created, err := store.ResolveMessage(ctx, abandon)
	if err != nil || !created || abandoned.ReservedWakeSequence != 0 {
		t.Fatalf("abandon receipt=%#v created=%v err=%v", abandoned, created, err)
	}
	job, err := store.Job(ctx, failedJob.ID)
	if err != nil || job.AdmissionOpen || job.CleanupState != spine.CleanupPending {
		t.Fatalf("abandoned job=%#v err=%v", job, err)
	}
	if _, _, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "after-abandon", Input: "must be closed"}); err == nil {
		t.Fatal("abandonment left admission open")
	}
}

func TestRetryChildFailureHasIndependentResolutionAndPreservesRootFIFO(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, sessionID := resolutionJob(t, store, "retry-child-failure-")
	failed := failNoSubmit(t, store, job.ID, sessionID)
	second, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "second", Input: "pending after the failed root"})
	if err != nil || !created || second.Sequence != 2 {
		t.Fatalf("second=%#v created=%v err=%v", second, created, err)
	}

	rootInput := postgres.MessageResolutionInput{JobID: job.ID, MessageID: failed.Message.ID, Decision: spine.ResolutionRetry, Authority: "operator", Reason: "native history proves the root input was not submitted"}
	rootReceipt, created, err := store.ResolveMessage(ctx, rootInput)
	if err != nil || !created || rootReceipt.ReservedWakeSequence != 3 || rootReceipt.RetryMessageID == "" {
		t.Fatalf("root receipt=%#v created=%v err=%v", rootReceipt, created, err)
	}
	fourth, created, err := store.AdmitMessage(ctx, postgres.NewMessage{JobID: job.ID, CallerID: "fourth", Input: "admitted after retry wake reservation"})
	if err != nil || !created || fourth.Sequence != 4 {
		t.Fatalf("fourth=%#v created=%v err=%v", fourth, created, err)
	}
	retryDelivery, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || retryDelivery == nil || retryDelivery.Message.ID != rootReceipt.RetryMessageID || retryDelivery.Message.Sequence != 3 || retryDelivery.Message.RetryOfMessageID != failed.Message.ID || retryDelivery.AgentRun.ID == failed.AgentRun.ID {
		t.Fatalf("root-priority retry delivery=%#v err=%v", retryDelivery, err)
	}
	if err := store.PrepareAgentRun(ctx, retryDelivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, retryDelivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeTurn(ctx, retryDelivery.AgentRun.ID, "failed-retry-turn-"+job.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	blockedWake, err := store.NextWakeSequence(ctx, job.ID)
	if err != nil || blockedWake != 5 {
		t.Fatalf("failed retry selected wake=%d want=5, not replayed wake 3: %v", blockedWake, err)
	}

	diagnosis, err := store.DiagnoseMessage(ctx, job.ID, retryDelivery.Message.ID)
	if err != nil || diagnosis.Message.RetryOfMessageID != failed.Message.ID || diagnosis.AgentRun.ID != retryDelivery.AgentRun.ID || diagnosis.NativeTurnID == "" || diagnosis.NativeOutcome != "failed" || fmt.Sprint(diagnosis.SafeDecisions) != "[acknowledge-loss abandon]" {
		t.Fatalf("retry child diagnosis=%#v err=%v", diagnosis, err)
	}
	original, err := store.DiagnoseMessage(ctx, job.ID, failed.Message.ID)
	if err != nil || original.Resolution == nil || *original.Resolution != rootReceipt || original.AgentRun.ID != failed.AgentRun.ID || original.AgentRun.State != spine.AgentRunFailed {
		t.Fatalf("preserved root diagnosis=%#v err=%v", original, err)
	}
	childReceipt, created, err := store.ResolveMessage(ctx, postgres.MessageResolutionInput{JobID: job.ID, MessageID: retryDelivery.Message.ID, Decision: spine.ResolutionAcknowledgeLoss, Authority: "operator", Reason: "preserve the failed retry turn and continue FIFO"})
	if err != nil || !created || childReceipt.MessageID != retryDelivery.Message.ID || childReceipt.ReservedWakeSequence != 5 {
		t.Fatalf("child receipt=%#v created=%v err=%v", childReceipt, created, err)
	}
	next, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || next == nil || next.Message.ID != second.ID {
		t.Fatalf("FIFO continuation after retry child resolution=%#v err=%v", next, err)
	}
	wake, err := store.NextWakeSequence(ctx, job.ID)
	if err != nil || wake != second.Sequence {
		t.Fatalf("replayed retry wake selected=%d want=%d err=%v", wake, second.Sequence, err)
	}
}

func TestReviewActivationUsesCanonicalSettlementAndResumesPendingIdentity(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, sessionID := resolutionJob(t, store, "review-resume-")
	failed := failNoSubmit(t, store, job.ID, sessionID)
	if _, err := store.DB.ExecContext(ctx, `update dorf.jobs set workflow_phase='review-activation' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	activation := spine.ReviewActivation{JobID: job.ID, Revision: job.Revision}
	if _, _, err := store.ActivateReview(ctx, activation); err == nil || !strings.Contains(err.Error(), "compatibility diagnosis") {
		t.Fatalf("unresolved activation error=%v", err)
	}
	var count int
	if err := store.DB.QueryRowContext(ctx, `select count(*) from dorf.review_plans where job_id=$1 and revision=$2`, job.ID, job.Revision).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unresolved activation partially persisted count=%d err=%v", count, err)
	}
	if _, err := store.DB.ExecContext(ctx, "insert into dorf.review_plans(job_id,revision,state,requested_roles) values($1,$2,'pending','[]'::jsonb)", job.ID, job.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, "update dorf.jobs set workflow_phase='blocked',workflow_attention='preserved FIFO sequence 1 as attention during review-planning' where id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.ReviewPlan(ctx, job.ID, job.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ResolveMessage(ctx, postgres.MessageResolutionInput{JobID: job.ID, MessageID: failed.Message.ID, Decision: spine.ResolutionAcknowledgeLoss, Authority: "owner", Reason: "preserve failed run and continue review"}); err != nil {
		t.Fatal(err)
	}
	replayed, created, err := store.ActivateReview(ctx, activation)
	if err != nil || created || replayed.CreatedAt != persisted.CreatedAt || replayed.JobID != persisted.JobID || replayed.Revision != persisted.Revision {
		t.Fatalf("pending activation replay=%#v created=%v err=%v", replayed, created, err)
	}
	recoveredJob, err := store.Job(ctx, job.ID)
	if err != nil || recoveredJob.WorkflowPhase != "review-planning" || recoveredJob.WorkflowAttention != "" {
		t.Fatalf("pending activation phase recovery=%#v err=%v", recoveredJob, err)
	}
	if err := store.DB.QueryRowContext(ctx, `select count(*) from dorf.review_plans where job_id=$1 and revision=$2`, job.ID, job.Revision).Scan(&count); err != nil || count != 1 {
		t.Fatalf("activation identity count=%d err=%v", count, err)
	}
}
