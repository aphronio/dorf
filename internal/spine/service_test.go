package spine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
)

func TestIntentionalCheckFailureGetsOneSameSessionRepairAndNewReadyRevision(t *testing.T) {
	base := newMemoryStore()
	store := &codingMemoryStore{memoryStore: base, checks: map[string]Check{}, evidence: map[string]Evidence{}}
	job := testJob()
	job.StartingRevision = job.Revision
	job.WorkflowPhase = "setup"
	store.jobs[job.ID] = job
	store.addMessage(job.ID, "initial", "make one bounded change and do not commit")
	externals := newFakeExternals()
	repository := &fakeRepository{firstRevision: strings.Repeat("a", 40), repairedRevision: strings.Repeat("b", 40)}
	service := Service{Store: store, Externals: externals, Repository: repository, Evidence: evidence.Store{Root: t.TempDir()}}
	disposition, err := service.RunUntilIdle(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := store.jobs[job.ID]
	if disposition != RunIdle || ready.WorkflowPhase != "ready" || ready.Revision != repository.repairedRevision || ready.RepairCount != 1 {
		t.Fatalf("disposition=%s job=%#v", disposition, ready)
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatalf("implementation and repair submissions=%v", got)
	}
	if len(store.messages[job.ID]) != 2 || store.messages[job.ID][1].CallerID != "dorf:repair:1" {
		t.Fatalf("repair FIFO=%#v", store.messages[job.ID])
	}
	firstRun := store.runs[AgentRunID(store.messages[job.ID][0].ID)]
	repairRun := store.runs[AgentRunID(store.messages[job.ID][1].ID)]
	if firstRun.SessionID == "" || repairRun.SessionID != firstRun.SessionID || repairRun.Role != "repair" {
		t.Fatalf("implementation=%#v repair=%#v", firstRun, repairRun)
	}
	if repository.setupCalls != 1 || repository.commitCalls != 2 || repository.checkCalls != 2 {
		t.Fatalf("repository calls setup=%d commit=%d check=%d", repository.setupCalls, repository.commitCalls, repository.checkCalls)
	}
	failed := store.checks[CheckID(job.ID, repository.firstRevision, "check")]
	passed := store.checks[CheckID(job.ID, repository.repairedRevision, "check")]
	if failed.State != "failed" || passed.State != "passed" || failed.EvidenceDigest == "" || passed.EvidenceDigest == "" || len(store.evidence) != 5 {
		t.Fatalf("failed=%#v passed=%#v Evidence=%#v", failed, passed, store.evidence)
	}
	if setup := store.actions[ActionID(job.ID, ActionRepositorySetup)]; setup.State != ActionSucceeded {
		t.Fatalf("setup Action=%#v", setup)
	}
}

func TestAdmissionDuringActiveTurnDrainsDistinctAgentRunsInFIFO(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	store.addMessage(job.ID, "first", "first input")
	externals := newFakeExternals()
	externals.blockFirst = make(chan struct{})
	externals.firstActive = make(chan struct{})
	service := Service{Store: store, Externals: externals}
	done := make(chan error, 1)
	go func() { done <- service.Run(context.Background(), job.ID) }()
	<-externals.firstActive
	second := store.addMessage(job.ID, "second", "identical text remains independent")
	close(externals.blockFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, want := externals.submittedSequences(), []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("native submission order=%v want=%v", got, want)
	}
	first := store.runs[AgentRunID(MessageID(job.ID, "first"))]
	secondRun := store.runs[AgentRunID(second.ID)]
	if first.ID == secondRun.ID || first.State != AgentRunCompleted || secondRun.State != AgentRunCompleted {
		t.Fatalf("per-input AgentRuns were not distinct and terminal: %#v %#v", first, secondRun)
	}
}

func TestFaultBoundariesRecoverOneNativeTurn(t *testing.T) {
	for _, point := range []string{BarrierBeforeSubmit, BarrierAfterSubmitBeforeBind, BarrierNativeActive} {
		t.Run(point, func(t *testing.T) {
			store := newMemoryStore()
			job := testJob()
			store.jobs[job.ID] = job
			message := store.addMessage(job.ID, "one", "one input")
			externals := newFakeExternals()
			barrier := &failBarrier{point: point}
			service := Service{Store: store, Externals: externals, Barrier: barrier}
			if err := service.Run(context.Background(), job.ID); !errors.Is(err, errBarrier) {
				t.Fatalf("first run error=%v, want barrier", err)
			}
			checkpointSession := store.actions[ActionID(job.ID, ActionSessionStart)]
			checkpointRun := store.runs[AgentRunID(message.ID)]
			switch point {
			case BarrierBeforeSubmit:
				if checkpointSession.State == ActionSucceeded || checkpointRun.NativeTurnID != "" || len(externals.submittedSequences()) != 0 {
					t.Fatalf("before-submit state session=%#v run=%#v submissions=%v", checkpointSession, checkpointRun, externals.submittedSequences())
				}
			case BarrierAfterSubmitBeforeBind:
				if checkpointSession.State == ActionSucceeded || checkpointRun.NativeTurnID != "" || !reflect.DeepEqual(externals.submittedSequences(), []int64{1}) {
					t.Fatalf("after-submit state session=%#v run=%#v submissions=%v", checkpointSession, checkpointRun, externals.submittedSequences())
				}
			case BarrierNativeActive:
				if checkpointSession.State != ActionSucceeded || checkpointRun.NativeTurnID == "" || !reflect.DeepEqual(externals.submittedSequences(), []int64{1}) {
					t.Fatalf("native-active state session=%#v run=%#v submissions=%v", checkpointSession, checkpointRun, externals.submittedSequences())
				}
			}
			service.Barrier = nil
			if err := service.Run(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
				t.Fatalf("native submissions=%v want one", got)
			}
			run := store.runs[AgentRunID(message.ID)]
			session := store.actions[ActionID(job.ID, ActionSessionStart)]
			if session.State != ActionSucceeded || session.ExternalID != "session-1" || run.SessionID != session.ExternalID || run.State != AgentRunCompleted || run.NativeTurnID == "" || !run.BaselineRecorded || run.BaselineTurnID != "" {
				t.Fatalf("recovered AgentRun=%#v", run)
			}
		})
	}
}

func TestSteerAcceptanceRecoversAfterAcknowledgementBeforeBind(t *testing.T) {
	base := newMemoryStore()
	store := &steeringMemoryStore{memoryStore: base}
	job := testJob()
	job.SessionID = "session-1"
	store.jobs[job.ID] = job
	targetTurnID := "turn-active-1"
	message := Message{ID: MessageID(job.ID, "steer-1"), JobID: job.ID, CallerID: "steer-1", Sequence: 2, Input: "correct active work", Intent: MessageSteer, TargetTurnID: targetTurnID}
	run := AgentRun{ID: AgentRunID(message.ID), JobID: job.ID, MessageID: message.ID, ActionID: TurnActionID(message.ID), SessionID: job.SessionID, State: AgentRunPending}
	store.messages[job.ID] = []Message{message}
	store.runs[run.ID] = run
	store.actions[run.ActionID] = Action{ID: run.ActionID, JobID: job.ID, MessageID: message.ID, Kind: ActionTurnStart, State: ActionPending}
	externals := &steeringFakeExternals{fakeExternals: newFakeExternals()}
	externals.turns = []NativeTurn{{ID: targetTurnID, Status: "inProgress"}}
	delivery := Delivery{Message: message, AgentRun: run}
	service := Service{Store: store, Externals: externals, Barrier: &failBarrier{point: BarrierAfterSubmitBeforeBind}}
	if _, err := service.deliverSteer(context.Background(), job, delivery); !errors.Is(err, errBarrier) {
		t.Fatalf("first steer error=%v want barrier", err)
	}
	if len(externals.steered) != 1 || store.runs[run.ID].NativeTurnID != "" {
		t.Fatalf("accepted-before-bind calls=%v run=%#v", externals.steered, store.runs[run.ID])
	}
	service.Barrier = nil
	progressed, err := service.deliverSteer(context.Background(), job, Delivery{Message: message, AgentRun: store.runs[run.ID]})
	if err != nil || !progressed {
		t.Fatalf("reconciled steer progressed=%v err=%v", progressed, err)
	}
	bound := store.runs[run.ID]
	if len(externals.steered) != 1 || bound.State != AgentRunCompleted || bound.NativeTurnID != targetTurnID {
		t.Fatalf("reconciled steer calls=%v run=%#v", externals.steered, bound)
	}
}

func TestSharedSteerPersistsEveryTerminalTargetOutcome(t *testing.T) {
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			base := newMemoryStore()
			store := &steeringMemoryStore{memoryStore: base}
			job := testJob()
			job.SessionID = "session-1"
			store.jobs[job.ID] = job
			targetTurnID := "turn-shared"
			target := Message{ID: MessageID(job.ID, "target"), JobID: job.ID, CallerID: "target", Sequence: 1, Input: "target input", Intent: MessageFollow}
			targetRun := AgentRun{ID: AgentRunID(target.ID), JobID: job.ID, MessageID: target.ID, ActionID: TurnActionID(target.ID), SessionID: job.SessionID, State: AgentRunActive, NativeTurnID: targetTurnID}
			steer := Message{ID: MessageID(job.ID, "steer-outcome"), JobID: job.ID, CallerID: "steer-outcome", Sequence: 2, Input: "accepted shared input", Intent: MessageSteer, TargetTurnID: targetTurnID}
			steerRun := AgentRun{ID: AgentRunID(steer.ID), JobID: job.ID, MessageID: steer.ID, ActionID: TurnActionID(steer.ID), SessionID: job.SessionID, State: AgentRunPending}
			store.messages[job.ID] = []Message{target, steer}
			store.runs[targetRun.ID], store.runs[steerRun.ID] = targetRun, steerRun
			store.actions[targetRun.ActionID] = Action{ID: targetRun.ActionID, JobID: job.ID, MessageID: target.ID, Kind: ActionTurnStart, State: ActionSucceeded, ExternalID: targetTurnID}
			store.actions[steerRun.ActionID] = Action{ID: steerRun.ActionID, JobID: job.ID, MessageID: steer.ID, Kind: ActionTurnStart, State: ActionPending}
			externals := &steeringFakeExternals{fakeExternals: newFakeExternals()}
			externals.turns = []NativeTurn{{ID: targetTurnID, Status: "inProgress"}}
			service := Service{Store: store, Externals: externals}
			if progressed, err := service.deliverSteer(context.Background(), job, Delivery{Message: steer, AgentRun: steerRun}); err != nil || !progressed {
				t.Fatalf("steer acceptance progressed=%v err=%v", progressed, err)
			}
			accepted := store.runs[steerRun.ID]
			if accepted.NativeTurnID != targetTurnID || accepted.NativeOutcome != "" || accepted.State != AgentRunCompleted {
				t.Fatalf("accepted steer=%#v", accepted)
			}
			externals.turns[0].Status = status
			if err := store.BindNativeTurn(context.Background(), targetRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			propagated := store.runs[steerRun.ID]
			if propagated.NativeOutcome != status || propagated.State != AgentRunCompleted {
				t.Fatalf("propagated steer=%#v", propagated)
			}

			propagated.NativeOutcome = ""
			store.runs[steerRun.ID] = propagated
			if progressed, err := service.deliver(context.Background(), job, Delivery{Message: steer, AgentRun: propagated}); err != nil || !progressed {
				t.Fatalf("terminal-history reconciliation progressed=%v err=%v", progressed, err)
			}
			if err := store.BindNativeTurn(context.Background(), targetRun.ID, targetTurnID, status); err != nil {
				t.Fatal(err)
			}
			if progressed, err := service.deliver(context.Background(), job, Delivery{Message: steer, AgentRun: store.runs[steerRun.ID]}); err != nil || !progressed {
				t.Fatalf("idempotent steer replay progressed=%v err=%v", progressed, err)
			}
			final := store.runs[steerRun.ID]
			if final.ID != steerRun.ID || final.ActionID != steerRun.ActionID || final.NativeTurnID != targetTurnID || final.NativeOutcome != status || len(externals.steered) != 1 || len(externals.submittedSequences()) != 0 {
				t.Fatalf("final steer=%#v steers=%v starts=%v", final, externals.steered, externals.submittedSequences())
			}
		})
	}
}

func TestSteerTerminalRaceStartsSameDurableInputAtMostOnce(t *testing.T) {
	tests := []struct {
		name           string
		targetStatus   string
		rejectOnSteer  bool
		lostStartReply bool
		wantSteers     int
	}{
		{name: "terminal before steer", targetStatus: "completed"},
		{name: "terminal during steer rejection", targetStatus: "inProgress", rejectOnSteer: true, lostStartReply: true, wantSteers: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := newMemoryStore()
			store := &steeringMemoryStore{memoryStore: base}
			job := testJob()
			job.SessionID = "session-1"
			store.jobs[job.ID] = job
			targetTurnID := "turn-original-target"
			target := Message{ID: MessageID(job.ID, "target"), JobID: job.ID, CallerID: "target", Sequence: 1, Input: "original work", Intent: MessageFollow}
			targetRun := AgentRun{ID: AgentRunID(target.ID), JobID: job.ID, MessageID: target.ID, ActionID: TurnActionID(target.ID), SessionID: job.SessionID, State: AgentRunCompleted, NativeTurnID: targetTurnID, NativeOutcome: "completed"}
			message := Message{ID: MessageID(job.ID, "steer-race"), JobID: job.ID, CallerID: "steer-race", Sequence: 2, Input: "preserve these exact bytes", Intent: MessageSteer, TargetTurnID: targetTurnID}
			run := AgentRun{ID: AgentRunID(message.ID), JobID: job.ID, MessageID: message.ID, ActionID: TurnActionID(message.ID), SessionID: job.SessionID, State: AgentRunPending}
			store.messages[job.ID] = []Message{target, message}
			store.runs[targetRun.ID], store.runs[run.ID] = targetRun, run
			store.actions[run.ActionID] = Action{ID: run.ActionID, JobID: job.ID, MessageID: message.ID, Kind: ActionTurnStart, State: ActionPending}
			externals := &steeringFakeExternals{fakeExternals: newFakeExternals(), rejectOnSteer: test.rejectOnSteer}
			externals.turns = []NativeTurn{{ID: targetTurnID, Status: test.targetStatus}}
			if test.lostStartReply {
				externals.submitError = errors.New("turn/start acknowledgement lost")
			}
			service := Service{Store: store, Externals: externals}
			if test.lostStartReply {
				service.Barrier = &failBarrier{point: BarrierAfterSubmitBeforeBind}
			}
			progressed, err := service.deliverSteer(context.Background(), job, Delivery{Message: message, AgentRun: run})
			if test.lostStartReply {
				if !errors.Is(err, errBarrier) || progressed {
					t.Fatalf("lost start acknowledgement progressed=%v err=%v", progressed, err)
				}
				if len(externals.steered) != test.wantSteers || !reflect.DeepEqual(externals.submittedSequences(), []int64{2}) || store.runs[run.ID].NativeTurnID != "" {
					t.Fatalf("pre-bind transport steers=%v starts=%v run=%#v", externals.steered, externals.submittedSequences(), store.runs[run.ID])
				}
				service.Barrier = nil
				progressed, err = service.deliver(context.Background(), job, Delivery{Message: message, AgentRun: store.runs[run.ID]})
			}
			if err != nil || !progressed {
				t.Fatalf("fallback recovery progressed=%v err=%v", progressed, err)
			}
			bound := store.runs[run.ID]
			actualTurnID := "turn-" + message.ID
			if bound.ID != run.ID || bound.ActionID != run.ActionID || bound.NativeTurnID != actualTurnID || bound.NativeOutcome != "completed" || bound.BaselineTurnID != targetTurnID {
				t.Fatalf("fallback run=%#v", bound)
			}
			if store.messages[job.ID][1] != message {
				t.Fatalf("admitted message mutated: got=%#v want=%#v", store.messages[job.ID][1], message)
			}
			if len(externals.steered) != test.wantSteers || !reflect.DeepEqual(externals.submittedSequences(), []int64{2}) {
				t.Fatalf("steers=%v starts=%v", externals.steered, externals.submittedSequences())
			}
			if progressed, err := service.deliver(context.Background(), job, Delivery{Message: message, AgentRun: bound}); err != nil || !progressed {
				t.Fatalf("bound replay progressed=%v err=%v", progressed, err)
			}
			if len(externals.steered) != test.wantSteers || !reflect.DeepEqual(externals.submittedSequences(), []int64{2}) {
				t.Fatalf("bound replay duplicated transport: steers=%v starts=%v", externals.steered, externals.submittedSequences())
			}

			externals.submitError = nil
			later := store.addMessage(job.ID, "later-follow", "later FIFO input")
			delivery, err := store.NextDelivery(context.Background(), job.ID, job.SessionID)
			if err != nil || delivery == nil || delivery.Message.ID != later.ID {
				t.Fatalf("later delivery=%#v err=%v", delivery, err)
			}
			if progressed, err := service.deliver(context.Background(), job, *delivery); err != nil || !progressed {
				t.Fatalf("later follow progressed=%v err=%v", progressed, err)
			}
			if !reflect.DeepEqual(externals.submittedSequences(), []int64{2, 3}) || store.runs[AgentRunID(later.ID)].State != AgentRunCompleted {
				t.Fatalf("later starts=%v run=%#v", externals.submittedSequences(), store.runs[AgentRunID(later.ID)])
			}
		})
	}
	t.Run("unbaselined suffix remains uncertain", func(t *testing.T) {
		base := newMemoryStore()
		store := &steeringMemoryStore{memoryStore: base}
		job := testJob()
		job.SessionID = "session-1"
		store.jobs[job.ID] = job
		targetTurnID := "turn-original-target"
		message := Message{ID: MessageID(job.ID, "ambiguous-steer"), JobID: job.ID, CallerID: "ambiguous-steer", Sequence: 2, Input: "exact input", Intent: MessageSteer, TargetTurnID: targetTurnID}
		run := AgentRun{ID: AgentRunID(message.ID), JobID: job.ID, MessageID: message.ID, ActionID: TurnActionID(message.ID), SessionID: job.SessionID, State: AgentRunPending}
		store.messages[job.ID], store.runs[run.ID] = []Message{message}, run
		store.actions[run.ActionID] = Action{ID: run.ActionID, JobID: job.ID, MessageID: message.ID, Kind: ActionTurnStart, State: ActionPending}
		externals := &steeringFakeExternals{fakeExternals: newFakeExternals()}
		externals.turns = []NativeTurn{{ID: targetTurnID, Status: "completed"}, {ID: "unexpected-suffix", Status: "completed"}}
		progressed, err := (Service{Store: store, Externals: externals}).deliverSteer(context.Background(), job, Delivery{Message: message, AgentRun: run})
		if err != nil || progressed || store.runs[run.ID].State != AgentRunUncertain || len(externals.steered) != 0 || len(externals.submittedSequences()) != 0 {
			t.Fatalf("progressed=%v err=%v run=%#v steers=%v starts=%v", progressed, err, store.runs[run.ID], externals.steered, externals.submittedSequences())
		}
	})
}

func TestRepositoryRecordBoundariesRecoverCompletedLogicalWork(t *testing.T) {
	for _, point := range []string{BarrierSetupComplete, BarrierCommitCreated, BarrierCheckExited} {
		t.Run(point, func(t *testing.T) {
			base := newMemoryStore()
			store := &codingMemoryStore{memoryStore: base, checks: map[string]Check{}, evidence: map[string]Evidence{}}
			job := testJob()
			job.StartingRevision, job.WorkflowPhase = job.Revision, "setup"
			store.jobs[job.ID] = job
			store.addMessage(job.ID, "initial", "make one bounded change")
			repository := &receiptRepository{revision: strings.Repeat("c", 40)}
			service := Service{Store: store, Externals: newFakeExternals(), Repository: repository, Evidence: evidence.Store{Root: t.TempDir()}, Barrier: &failWorkflowBarrier{point: point}}
			if err := service.Run(context.Background(), job.ID); !errors.Is(err, errBarrier) {
				t.Fatalf("first run error=%v", err)
			}
			if err := service.Run(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			ready := store.jobs[job.ID]
			if ready.WorkflowPhase != "ready" || repository.setupExecutions != 1 || repository.commitExecutions != 1 || repository.checkExecutions != 1 {
				t.Fatalf("ready=%#v logical executions setup=%d commit=%d check=%d", ready, repository.setupExecutions, repository.commitExecutions, repository.checkExecutions)
			}
		})
	}
}

func TestInitialRecoveryAdoptsTurnAfterSessionCheckpoint(t *testing.T) {
	store := newMemoryStore()
	store.bindFailures = 1
	job := testJob()
	store.jobs[job.ID] = job
	message := store.addMessage(job.ID, "one", "one input")
	externals := newFakeExternals()
	service := Service{Store: store, Externals: externals}
	if err := service.Run(context.Background(), job.ID); err == nil || !strings.Contains(err.Error(), "checkpoint native turn") {
		t.Fatalf("first run error=%v", err)
	}
	session := store.actions[ActionID(job.ID, ActionSessionStart)]
	run := store.runs[AgentRunID(message.ID)]
	if session.State != ActionSucceeded || session.ExternalID != "session-1" || run.SessionID != "session-1" || run.NativeTurnID != "" {
		t.Fatalf("partial checkpoint session=%#v run=%#v", session, run)
	}
	if err := service.Run(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("native submissions=%v want one", got)
	}
	run = store.runs[AgentRunID(message.ID)]
	if run.State != AgentRunCompleted || run.NativeTurnID == "" {
		t.Fatalf("recovered AgentRun=%#v", run)
	}
}

func TestAcceptedFailedAndInterruptedTurnsPermitLaterFIFO(t *testing.T) {
	for _, status := range []string{"failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			store := newMemoryStore()
			job := testJob()
			store.jobs[job.ID] = job
			first := store.addMessage(job.ID, "first", "first")
			store.addMessage(job.ID, "second", "second")
			externals := newFakeExternals()
			externals.outcomes[1] = status
			disposition, err := (Service{Store: store, Externals: externals}).RunUntilIdle(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if disposition != RunIdle || !reflect.DeepEqual(externals.submittedSequences(), []int64{1, 2}) {
				t.Fatalf("disposition=%s submissions=%v", disposition, externals.submittedSequences())
			}
			firstRun := store.runs[AgentRunID(first.ID)]
			if string(firstRun.State) != status || firstRun.NativeTurnID == "" || firstRun.NativeOutcome != status {
				t.Fatalf("first run=%#v want preserved accepted %s outcome", firstRun, status)
			}
		})
	}
}

func TestAmbiguousNativeSuffixPersistsAttentionWithoutResubmission(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	store.addMessage(job.ID, "first", "first")
	delivery, err := store.NextDelivery(context.Background(), job.ID, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareAgentRun(context.Background(), delivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginAction(context.Background(), job.ID, ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAction(context.Background(), session.ID, Receipt{ExternalID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	externals := newFakeExternals()
	externals.turns = []NativeTurn{{ID: "unbound-a", Status: "completed"}, {ID: "unbound-b", Status: "running"}}
	disposition, err := (Service{Store: store, Externals: externals}).RunUntilIdle(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := store.runs[delivery.AgentRun.ID]
	if disposition != RunBlocked || run.State != AgentRunUncertain || !strings.Contains(run.Attention, "2 native turns") || len(externals.submittedSequences()) != 0 {
		t.Fatalf("disposition=%s run=%#v submissions=%v", disposition, run, externals.submittedSequences())
	}
}

func TestUnsupportedNativeStatusPersistsAttentionAndBlocksFIFO(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	first := store.addMessage(job.ID, "first", "first")
	store.addMessage(job.ID, "second", "second")
	externals := newFakeExternals()
	externals.outcomes[1] = "pausedByFutureServer"
	disposition, err := (Service{Store: store, Externals: externals}).RunUntilIdle(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := store.runs[AgentRunID(first.ID)]
	if disposition != RunBlocked || run.State != AgentRunUncertain || !strings.Contains(run.Attention, "unsupported status") {
		t.Fatalf("disposition=%s run=%#v", disposition, run)
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("unsupported status allowed later FIFO submission: %v", got)
	}
}

func TestOverlappingClaimsSerializeNativeMutation(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	store.addMessage(job.ID, "first", "first")
	externals := newFakeExternals()
	externals.blockFirst = make(chan struct{})
	externals.firstActive = make(chan struct{})
	service := Service{Store: store, Externals: externals}
	errs := make(chan error, 2)
	go func() { errs <- service.Run(context.Background(), job.ID) }()
	<-externals.firstActive
	go func() { errs <- service.Run(context.Background(), job.ID) }()
	select {
	case <-externals.secondClaim:
		t.Fatal("second claim crossed the Job fence")
	case <-time.After(100 * time.Millisecond):
	}
	close(externals.blockFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("submissions=%v", got)
	}
}

func TestCleanupUsesSameFenceAndIsIdempotent(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	job.AdmissionOpen = false
	job.RouteID, job.SandboxID = "route-exact", "sandbox-exact"
	store.jobs[job.ID] = job
	externals := newFakeExternals()
	service := Service{Store: store, Externals: externals}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := externals.effects, []ActionKind{ActionRouteRevoke, ActionSandboxDelete}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup effects=%v want=%v", got, want)
	}
	if store.jobs[job.ID].CleanupAttention != "" {
		t.Fatalf("completed cleanup retained stale attention: %q", store.jobs[job.ID].CleanupAttention)
	}
}

func TestCleanupSkipsMainResourcesWhoseCreateIntentWasNeverRecorded(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	job.AdmissionOpen = false
	store.jobs[job.ID] = job
	externals := newFakeExternals()
	service := Service{Store: store, Externals: externals}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if len(externals.effects) != 0 || store.jobs[job.ID].CleanupState != CleanupComplete {
		t.Fatalf("cleanup effects=%v Job=%#v", externals.effects, store.jobs[job.ID])
	}
}

func TestMainCleanupSuccessBeforeRecordBarriersRetainPartialInventoryAndSkipSettledEffects(t *testing.T) {
	for _, test := range []struct {
		point         string
		firstEffects  []ActionKind
		finalEffects  []ActionKind
		attentionPart string
	}{
		{BarrierMainRouteRevoked, []ActionKind{ActionRouteRevoke}, []ActionKind{ActionRouteRevoke, ActionRouteRevoke, ActionSandboxDelete}, "main provider route"},
		{BarrierMainSandboxDeleted, []ActionKind{ActionRouteRevoke, ActionSandboxDelete}, []ActionKind{ActionRouteRevoke, ActionSandboxDelete, ActionSandboxDelete}, "main Sandbox"},
	} {
		t.Run(test.point, func(t *testing.T) {
			store := newMemoryStore()
			job := testJob()
			job.AdmissionOpen, job.CleanupState = false, CleanupScheduled
			job.RouteID, job.SandboxID = "route-exact", "sandbox-exact"
			store.jobs[job.ID] = job
			externals := newFakeExternals()
			barrier := &failWorkflowBarrier{point: test.point}
			service := Service{Store: store, Externals: externals, Barrier: barrier}
			if err := service.Cleanup(context.Background(), job.ID); !errors.Is(err, errBarrier) {
				t.Fatalf("first cleanup error=%v", err)
			}
			if !reflect.DeepEqual(externals.effects, test.firstEffects) || !strings.Contains(store.jobs[job.ID].CleanupAttention, test.attentionPart) || store.jobs[job.ID].CleanupState == CleanupComplete {
				t.Fatalf("partial effects=%v Job=%#v", externals.effects, store.jobs[job.ID])
			}
			if err := service.Cleanup(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(externals.effects, test.finalEffects) || store.jobs[job.ID].CleanupState != CleanupComplete || store.jobs[job.ID].CleanupAttention != "" {
				t.Fatalf("converged effects=%v Job=%#v", externals.effects, store.jobs[job.ID])
			}
		})
	}
}

func TestCleanupRecoversCompletedTurnAfterRunTaskFailed(t *testing.T) {
	store, job, delivery := cleanupDelivery(t, AgentRunActive)
	store.jobs[job.ID] = job
	delivery.AgentRun.NativeTurnID = "turn-existing"
	store.runs[delivery.AgentRun.ID] = delivery.AgentRun
	externals := newFakeExternals()
	externals.turns = []NativeTurn{{ID: "turn-existing", Status: "completed"}}
	externals.submitted = []int64{1}

	if err := (Service{Store: store, Externals: externals}).Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	run := store.runs[delivery.AgentRun.ID]
	if run.State != AgentRunCompleted || run.NativeOutcome != "completed" || run.NativeTurnID != "turn-existing" {
		t.Fatalf("recovered run=%#v", run)
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("cleanup resubmitted native input: %v", got)
	}
	if got, want := externals.effects, []ActionKind{ActionRouteRevoke, ActionSandboxDelete}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup effects=%v want=%v", got, want)
	}
}

func TestCleanupAdoptsInitialTurnBeforeSessionCheckpoint(t *testing.T) {
	store, job, delivery := cleanupDelivery(t, AgentRunSubmitting)
	job.SessionID = ""
	store.jobs[job.ID] = job
	delivery.AgentRun.SessionID = ""
	store.runs[delivery.AgentRun.ID] = delivery.AgentRun
	externals := newFakeExternals()
	externals.initialSessionID = "session-recovered"
	externals.turns = []NativeTurn{{ID: "turn-recovered", Status: "completed"}}
	externals.submitted = []int64{1}

	if err := (Service{Store: store, Externals: externals}).Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	run := store.runs[delivery.AgentRun.ID]
	session := store.actions[ActionID(job.ID, ActionSessionStart)]
	if session.State != ActionSucceeded || session.ExternalID != "session-recovered" || run.SessionID != session.ExternalID || run.NativeTurnID != "turn-recovered" || run.State != AgentRunCompleted {
		t.Fatalf("recovered session=%#v run=%#v", session, run)
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("cleanup resubmitted initial input: %v", got)
	}
}

func TestCleanupTerminalizesProvenNoSubmitWithoutCallingAgent(t *testing.T) {
	store, job, delivery := cleanupDelivery(t, AgentRunSubmitting)
	job.SessionID = ""
	store.jobs[job.ID] = job
	delivery.AgentRun.SessionID = ""
	store.runs[delivery.AgentRun.ID] = delivery.AgentRun
	externals := newFakeExternals()
	if err := (Service{Store: store, Externals: externals}).Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	run := store.runs[delivery.AgentRun.ID]
	if run.State != AgentRunFailed || run.NativeTurnID != "" || run.NativeOutcome != "" || !strings.Contains(run.Attention, "proved no turn was submitted") {
		t.Fatalf("locally terminal run=%#v", run)
	}
	if len(externals.submittedSequences()) != 0 {
		t.Fatalf("cleanup submitted native input: %v", externals.submittedSequences())
	}
}

func TestCleanupDoesNotSubmitLaterPendingFIFO(t *testing.T) {
	store, job, delivery := cleanupDelivery(t, AgentRunActive)
	delivery.AgentRun.NativeTurnID = "turn-existing"
	store.runs[delivery.AgentRun.ID] = delivery.AgentRun
	second := store.addMessage(job.ID, "second", "must remain pending")
	secondRun := AgentRun{ID: AgentRunID(second.ID), JobID: job.ID, MessageID: second.ID, ActionID: TurnActionID(second.ID), SessionID: "session-1", State: AgentRunPending}
	store.runs[secondRun.ID] = secondRun
	store.actions[secondRun.ActionID] = Action{ID: secondRun.ActionID, JobID: job.ID, MessageID: second.ID, Kind: ActionTurnStart, State: ActionPending}
	externals := newFakeExternals()
	externals.turns = []NativeTurn{{ID: "turn-existing", Status: "completed"}}
	externals.submitted = []int64{1}

	if err := (Service{Store: store, Externals: externals}).Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got := store.runs[secondRun.ID]; got.State != AgentRunPending || got.NativeTurnID != "" {
		t.Fatalf("later FIFO mutated=%#v", got)
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("cleanup drained later FIFO: %v", got)
	}
}

func TestCleanupRetainsResourcesWhenNativeHistoryIsUncertain(t *testing.T) {
	store, job, delivery := cleanupDelivery(t, AgentRunSubmitting)
	externals := newFakeExternals()
	externals.turns = []NativeTurn{{ID: "ambiguous-a", Status: "completed"}, {ID: "ambiguous-b", Status: "completed"}}
	err := (Service{Store: store, Externals: externals}).Cleanup(context.Background(), job.ID)
	if err == nil || !strings.Contains(err.Error(), "retained Sandbox and route") || !strings.Contains(err.Error(), "sequence 1") {
		t.Fatalf("cleanup error=%v", err)
	}
	run := store.runs[delivery.AgentRun.ID]
	if run.State != AgentRunUncertain || !strings.Contains(run.Attention, "2 native turns") || len(externals.effects) != 0 || store.jobs[job.ID].CleanupState == CleanupComplete {
		t.Fatalf("uncertain cleanup run=%#v effects=%v job=%#v", run, externals.effects, store.jobs[job.ID])
	}
	if attention := store.jobs[job.ID].CleanupAttention; !strings.Contains(attention, "implementation native mutation") || !strings.Contains(attention, "2 native turns") {
		t.Fatalf("partial cleanup diagnostic=%q", attention)
	}
}

func cleanupDelivery(t *testing.T, state AgentRunState) (*memoryStore, Job, Delivery) {
	t.Helper()
	store := newMemoryStore()
	job := testJob()
	job.AdmissionOpen = false
	job.CleanupState = CleanupScheduled
	job.SessionID = "session-1"
	job.RouteID, job.SandboxID = "route-exact", "sandbox-exact"
	store.jobs[job.ID] = job
	message := store.addMessage(job.ID, "first", "first input")
	delivery, err := store.NextDelivery(context.Background(), job.ID, job.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	run := delivery.AgentRun
	run.State = state
	run.BaselineRecorded = true
	store.runs[AgentRunID(message.ID)] = run
	delivery.AgentRun = run
	return store, job, *delivery
}

func TestBaselineReconciliationClassifications(t *testing.T) {
	turns := []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "running"}}
	tests := []struct {
		name               string
		recorded           bool
		baseline, known    string
		turns              []NativeTurn
		wantClassification string
	}{
		{"no submit", true, "before", "", turns[:1], "no-submit"},
		{"submitted active", true, "before", "", turns, "active"},
		{"submitted app-server active", true, "before", "", []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "inProgress"}}, "active"},
		{"submitted completed before bind", true, "before", "", []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "completed"}}, "completed"},
		{"known completed", true, "before", "logical", []NativeTurn{{ID: "logical", Status: "completed"}}, "completed"},
		{"known failed", true, "before", "logical", []NativeTurn{{ID: "logical", Status: "failed"}}, "failed"},
		{"known interrupted", true, "before", "logical", []NativeTurn{{ID: "logical", Status: "interrupted"}}, "interrupted"},
		{"multiple suffix", true, "before", "", append(turns, NativeTurn{ID: "other", Status: "running"}), "uncertain"},
		{"missing baseline", true, "gone", "", turns, "uncertain"},
		{"missing known", true, "before", "logical", turns[:1], "uncertain"},
		{"unknown status", true, "before", "", []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "pausedByFutureServer"}}, "uncertain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ReconcileTurns(test.recorded, test.baseline, test.known, test.turns)
			if got.Classification != test.wantClassification {
				t.Fatalf("classification=%s reason=%s", got.Classification, got.Reason)
			}
		})
	}
}

var errBarrier = errors.New("proof barrier")

type failBarrier struct {
	point  string
	failed bool
}

type failWorkflowBarrier struct {
	point  string
	failed bool
}

func (b *failWorkflowBarrier) Reach(context.Context, string, Delivery) error { return nil }

func (b *failWorkflowBarrier) ReachWorkflow(_ context.Context, point, _, _ string) error {
	if point == b.point && !b.failed {
		b.failed = true
		return errBarrier
	}
	return nil
}

func (b *failBarrier) Reach(_ context.Context, point string, _ Delivery) error {
	if point == b.point && !b.failed {
		b.failed = true
		return errBarrier
	}
	return nil
}

type memoryStore struct {
	mu           sync.Mutex
	fence        sync.Mutex
	jobs         map[string]Job
	messages     map[string][]Message
	runs         map[string]AgentRun
	actions      map[string]Action
	bindFailures int
}

type steeringMemoryStore struct{ *memoryStore }

func (s *steeringMemoryStore) BindNativeSteer(_ context.Context, runID, turnID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	if run.NativeTurnID != "" && run.NativeTurnID != turnID {
		return errors.New("steer native turn conflict")
	}
	run.NativeTurnID, run.State = turnID, AgentRunCompleted
	if terminalNative(status) {
		if run.NativeOutcome != "" && run.NativeOutcome != status {
			return errors.New("steer native outcome conflict")
		}
		run.NativeOutcome = status
	}
	s.runs[runID] = run
	action := s.actions[run.ActionID]
	action.State, action.ExternalID, action.Outcome = ActionSucceeded, turnID, "steered"
	s.actions[run.ActionID] = action
	return nil
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]Job{}, messages: map[string][]Message{}, runs: map[string]AgentRun{}, actions: map[string]Action{}}
}

func (s *memoryStore) addMessage(jobID, callerID, input string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	message := Message{ID: MessageID(jobID, callerID), JobID: jobID, CallerID: callerID, Sequence: int64(len(s.messages[jobID]) + 1), Input: input, Intent: MessageFollow}
	s.messages[jobID] = append(s.messages[jobID], message)
	return message
}

func (s *memoryStore) Job(_ context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id], nil
}
func (s *memoryStore) WithJobFence(_ context.Context, _ string, fn func() error) error {
	s.fence.Lock()
	defer s.fence.Unlock()
	return fn()
}
func (s *memoryStore) BeginAction(_ context.Context, jobID string, kind ActionKind) (Action, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := ActionID(jobID, kind)
	if action, ok := s.actions[id]; ok {
		return action, nil
	}
	action := Action{ID: id, JobID: jobID, Kind: kind, State: ActionPending}
	s.actions[id] = action
	return action, nil
}
func (s *memoryStore) BeginSetup(ctx context.Context, jobID string) (Action, error) {
	return s.BeginAction(ctx, jobID, ActionRepositorySetup)
}
func (s *memoryStore) CompleteAction(_ context.Context, id string, receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[id]
	action.State, action.ExternalID, action.Outcome = ActionSucceeded, receipt.ExternalID, receipt.Outcome
	s.actions[id] = action
	job := s.jobs[action.JobID]
	if action.Kind == ActionSessionStart {
		job.SessionID = receipt.ExternalID
		for id, run := range s.runs {
			if run.JobID == action.JobID && run.SessionID == "" {
				run.SessionID = receipt.ExternalID
				s.runs[id] = run
			}
		}
	}
	s.jobs[action.JobID] = job
	return nil
}
func (s *memoryStore) UncertainAction(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[id]
	action.State = ActionUncertain
	s.actions[id] = action
	return nil
}
func (s *memoryStore) UncertainReviewSubmission(_ context.Context, runID, sessionActionID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	outcome := ReviewSubmissionUncertainOutcome + ": " + reason
	session := s.actions[sessionActionID]
	session.State, session.Outcome = ActionUncertain, outcome
	s.actions[sessionActionID] = session
	turn := s.actions[run.ActionID]
	turn.State, turn.Outcome = ActionUncertain, outcome
	s.actions[run.ActionID] = turn
	run.State, run.Attention = AgentRunUncertain, reason
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) NextDelivery(_ context.Context, jobID, sessionID string) (*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := append([]Message(nil), s.messages[jobID]...)
	sort.Slice(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
	for _, message := range messages {
		runID := AgentRunID(message.ID)
		run, ok := s.runs[runID]
		if ok && run.NativeTurnID != "" && (run.State == AgentRunCompleted || run.State == AgentRunFailed || run.State == AgentRunInterrupted) {
			continue
		}
		if !ok {
			actionID := TurnActionID(message.ID)
			run = AgentRun{ID: runID, JobID: jobID, MessageID: message.ID, ActionID: actionID, SessionID: sessionID, State: AgentRunPending}
			s.runs[runID] = run
			s.actions[actionID] = Action{ID: actionID, JobID: jobID, MessageID: message.ID, Kind: ActionTurnStart, State: ActionPending}
		} else if sessionID != "" && run.SessionID == "" {
			run.SessionID = sessionID
			s.runs[runID] = run
		}
		return &Delivery{Message: message, AgentRun: run}, nil
	}
	return nil, nil
}
func (s *memoryStore) PrepareAgentRun(_ context.Context, runID, baseline string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.State, run.BaselineRecorded, run.BaselineTurnID = AgentRunSubmitting, true, baseline
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) BeginTurnSubmission(_ context.Context, runID string) error { return nil }
func (s *memoryStore) BindNativeTurn(_ context.Context, runID, turnID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bindFailures > 0 {
		s.bindFailures--
		return errors.New("checkpoint native turn")
	}
	run := s.runs[runID]
	run.NativeTurnID = turnID
	switch status {
	case "completed":
		run.State, run.NativeOutcome = AgentRunCompleted, status
	case "failed":
		run.State, run.NativeOutcome = AgentRunFailed, status
	case "interrupted":
		run.State, run.NativeOutcome = AgentRunInterrupted, status
	case "running", "inProgress":
		run.State = AgentRunActive
	default:
		run.State = AgentRunUncertain
		run.Attention = "native turn " + turnID + " has unsupported status \"" + status + "\""
	}
	s.runs[runID] = run
	if terminalNative(status) {
		for _, message := range s.messages[run.JobID] {
			accepted := s.runs[AgentRunID(message.ID)]
			if accepted.ID != runID && message.Intent == MessageSteer && message.TargetTurnID == turnID && accepted.NativeTurnID == turnID && accepted.State == AgentRunCompleted && accepted.NativeOutcome == "" {
				accepted.NativeOutcome = status
				s.runs[accepted.ID] = accepted
			}
		}
	}
	action := s.actions[run.ActionID]
	action.State, action.ExternalID = ActionSucceeded, turnID
	s.actions[run.ActionID] = action
	return nil
}
func (s *memoryStore) FailAgentRun(_ context.Context, runID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.State, run.Attention = AgentRunFailed, reason
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) UncertainAgentRun(_ context.Context, runID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.State, run.Attention = AgentRunUncertain, reason
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) AgentRunAttention(_ context.Context, runID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.Attention = reason
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) NativeMutationDelivery(_ context.Context, jobID string) (*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := append([]Message(nil), s.messages[jobID]...)
	sort.Slice(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
	for _, message := range messages {
		run, ok := s.runs[AgentRunID(message.ID)]
		if ok && (run.State == AgentRunSubmitting || run.State == AgentRunActive || run.State == AgentRunUncertain) {
			return &Delivery{Message: message, AgentRun: run}, nil
		}
	}
	return nil, nil
}
func (s *memoryStore) SetCleanupAttention(_ context.Context, jobID, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	job.CleanupAttention = detail
	s.jobs[jobID] = job
	return nil
}
func (s *memoryStore) CompleteCleanup(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	job.CleanupState = CleanupComplete
	job.CleanupAttention = ""
	s.jobs[jobID] = job
	return nil
}

type codingMemoryStore struct {
	*memoryStore
	checks   map[string]Check
	evidence map[string]Evidence
	declared []DeclaredCheck
}

func (s *codingMemoryStore) BeginCommit(_ context.Context, jobID, scope string) (Action, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	var latestFollow AgentRun
	for _, message := range s.messages[jobID] {
		run := s.runs[AgentRunID(message.ID)]
		if run.NativeTurnID == "" || (run.State != AgentRunCompleted && run.State != AgentRunFailed && run.State != AgentRunInterrupted) {
			return Action{}, false, nil
		}
		if message.Intent == MessageFollow {
			latestFollow = run
		}
	}
	if latestFollow.State != AgentRunCompleted {
		return Action{}, false, nil
	}
	if job.WorkflowPhase != "implementing" && job.WorkflowPhase != "repairing" && job.WorkflowPhase != "committing" {
		return Action{}, false, fmt.Errorf("commit during %s", job.WorkflowPhase)
	}
	job.WorkflowPhase = "committing"
	s.jobs[jobID] = job
	id := ScopedActionID(jobID, ActionRepositoryCommit, scope)
	if action, ok := s.actions[id]; ok {
		return action, true, nil
	}
	action := Action{ID: id, JobID: jobID, Kind: ActionRepositoryCommit, State: ActionPending, Scope: scope}
	s.actions[id] = action
	return action, true, nil
}

func (s *codingMemoryStore) RecordSetup(_ context.Context, actionID string, record Evidence, observation CommandObservation, declared []DeclaredCheck) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[actionID]
	action.ExternalID = record.Digest
	job := s.jobs[action.JobID]
	if observation.ExitCode == 0 {
		action.State, job.WorkflowPhase = ActionSucceeded, "implementing"
	} else {
		action.State, job.WorkflowPhase = ActionFailed, "blocked"
	}
	s.actions[actionID], s.jobs[job.ID], s.evidence[record.ID] = action, job, record
	s.declared = append([]DeclaredCheck(nil), declared...)
	return nil
}

func (s *codingMemoryStore) DeclaredChecks(_ context.Context, _ string) ([]DeclaredCheck, error) {
	return append([]DeclaredCheck(nil), s.declared...), nil
}

func (s *codingMemoryStore) RecordRevision(_ context.Context, actionID string, observation CommitObservation, record Evidence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[actionID]
	action.State, action.ExternalID = ActionSucceeded, observation.Revision
	job := s.jobs[action.JobID]
	if job.Revision != observation.Parent {
		return errors.New("revision parent conflict")
	}
	job.Revision, job.WorkflowPhase = observation.Revision, "checking"
	s.actions[actionID], s.jobs[job.ID], s.evidence[record.ID] = action, job, record
	return nil
}

func (s *codingMemoryStore) BeginCheck(_ context.Context, jobID, revision, name, command string) (Check, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := CheckID(jobID, revision, name)
	if check, ok := s.checks[id]; ok {
		return check, nil
	}
	check := Check{ID: id, JobID: jobID, Revision: revision, Name: name, Command: command, State: "running"}
	s.checks[id] = check
	return check, nil
}

func (s *codingMemoryStore) RecordCheck(_ context.Context, check Check, record Evidence, observation CommandObservation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	check.ExitCode, check.EvidenceID, check.EvidenceDigest = observation.ExitCode, record.ID, record.Digest
	check.StartedAt, check.FinishedAt = observation.StartedAt, observation.FinishedAt
	check.State = "passed"
	if observation.ExitCode != 0 {
		check.State = "failed"
	}
	s.checks[check.ID], s.evidence[record.ID] = check, record
	return nil
}

func (s *codingMemoryStore) AdmitRepair(_ context.Context, check Check) (Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[check.JobID]
	if job.RepairCount != 0 {
		for _, message := range s.messages[job.ID] {
			if message.CallerID == "dorf:repair:1" {
				return message, false, nil
			}
		}
	}
	message := Message{ID: MessageID(job.ID, "dorf:repair:1"), JobID: job.ID, CallerID: "dorf:repair:1", Sequence: int64(len(s.messages[job.ID]) + 1), Input: "focused failed Check repair", Intent: MessageFollow}
	s.messages[job.ID] = append(s.messages[job.ID], message)
	actionID, runID := TurnActionID(message.ID), AgentRunID(message.ID)
	s.actions[actionID] = Action{ID: actionID, JobID: job.ID, MessageID: message.ID, Kind: ActionTurnStart, State: ActionPending}
	s.runs[runID] = AgentRun{ID: runID, JobID: job.ID, MessageID: message.ID, ActionID: actionID, SessionID: job.SessionID, Role: "repair", State: AgentRunPending}
	job.RepairCount, job.WorkflowPhase = 1, "repairing"
	s.jobs[job.ID] = job
	return message, true, nil
}

func (s *codingMemoryStore) MarkReady(_ context.Context, jobID, revision string, verified []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	if job.Revision != revision {
		return errors.New("ready revision conflict")
	}
	if len(verified) != len(s.declared) {
		return errors.New("ready Evidence conflict")
	}
	job.WorkflowPhase = "ready"
	s.jobs[jobID] = job
	return nil
}

func (s *codingMemoryStore) Checks(_ context.Context, _ string) ([]Check, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	checks := make([]Check, 0, len(s.checks))
	for _, check := range s.checks {
		checks = append(checks, check)
	}
	return checks, nil
}

func (s *codingMemoryStore) Evidence(_ context.Context, _ string) ([]Evidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]Evidence, 0, len(s.evidence))
	for _, record := range s.evidence {
		records = append(records, record)
	}
	return records, nil
}

func (s *codingMemoryStore) BlockWorkflow(_ context.Context, jobID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	job.WorkflowPhase, job.WorkflowAttention = "blocked", reason
	s.jobs[jobID] = job
	return nil
}

type fakeRepository struct {
	setupCalls, commitCalls, checkCalls int
	firstRevision, repairedRevision     string
}

type receiptRepository struct {
	revision                                           string
	setupExecutions, commitExecutions, checkExecutions int
	setupObservation                                   *CommandObservation
	commitObservation                                  *CommitObservation
	commitArtifact                                     []byte
	checkObservations                                  map[string]CommandObservation
}

func (r *receiptRepository) RepositorySetup(_ context.Context, _ Job, _ Action) (CommandObservation, []DeclaredCheck, error) {
	if r.setupObservation == nil {
		r.setupExecutions++
		now := time.Now().UTC()
		observation := CommandObservation{Command: "prepare", StartedAt: now, FinishedAt: now}
		r.setupObservation = &observation
	}
	return *r.setupObservation, []DeclaredCheck{{Name: "check", Command: "go test ./..."}}, nil
}

func (r *receiptRepository) RepositoryCommit(_ context.Context, job Job, _ Action) (CommitObservation, []byte, error) {
	if r.commitObservation == nil {
		r.commitExecutions++
		now := time.Now().UTC()
		observation := CommitObservation{Parent: job.Revision, Revision: r.revision, Tree: strings.Repeat("d", 40), Branch: job.Branch, StartedAt: now, FinishedAt: now}
		artifact, _ := json.Marshal(observation)
		r.commitObservation, r.commitArtifact = &observation, artifact
	}
	return *r.commitObservation, append([]byte(nil), r.commitArtifact...), nil
}

func (r *receiptRepository) RepositoryCheck(_ context.Context, _ Job, check Check) (CommandObservation, error) {
	if r.checkObservations == nil {
		r.checkObservations = map[string]CommandObservation{}
	}
	observation, ok := r.checkObservations[check.ID]
	if !ok {
		r.checkExecutions++
		now := time.Now().UTC()
		observation = CommandObservation{Command: check.Command, StartedAt: now, FinishedAt: now}
		r.checkObservations[check.ID] = observation
	}
	return observation, nil
}

func (r *fakeRepository) RepositorySetup(_ context.Context, _ Job, _ Action) (CommandObservation, []DeclaredCheck, error) {
	r.setupCalls++
	now := time.Now().UTC()
	return CommandObservation{Command: "prepare", StartedAt: now, FinishedAt: now, Stdout: []byte("setup complete")}, []DeclaredCheck{{Name: "check", Command: "go test ./..."}}, nil
}

func (r *fakeRepository) RepositoryCommit(_ context.Context, job Job, _ Action) (CommitObservation, []byte, error) {
	r.commitCalls++
	revision := r.firstRevision
	if job.RepairCount == 1 {
		revision = r.repairedRevision
	}
	observation := CommitObservation{Parent: job.Revision, Revision: revision, Tree: strings.Repeat(fmt.Sprintf("%x", r.commitCalls), 40), Branch: job.Branch}
	artifact, _ := json.Marshal(observation)
	return observation, artifact, nil
}

func (r *fakeRepository) RepositoryCheck(_ context.Context, job Job, check Check) (CommandObservation, error) {
	r.checkCalls++
	now := time.Now().UTC()
	exitCode := 0
	if job.Revision == r.firstRevision {
		exitCode = 1
	}
	return CommandObservation{Command: check.Command, ExitCode: exitCode, StartedAt: now, FinishedAt: now, Stdout: []byte("deterministic Check")}, nil
}

type fakeExternals struct {
	mu               sync.Mutex
	turns            []NativeTurn
	submitted        []int64
	outcomes         map[int64]string
	effects          []ActionKind
	blockFirst       chan struct{}
	firstActive      chan struct{}
	activeOnce       sync.Once
	secondClaim      chan struct{}
	initialSessionID string
	submitError      error
}

type steeringFakeExternals struct {
	*fakeExternals
	steered       []string
	rejectOnSteer bool
}

func (e *steeringFakeExternals) AgentSteer(_ context.Context, _ Job, delivery Delivery) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.steered = append(e.steered, delivery.AgentRun.ID)
	for index := range e.turns {
		if e.turns[index].ID == delivery.Message.TargetTurnID {
			if e.rejectOnSteer {
				e.turns[index].Status = "completed"
				return "", errors.New("turn/steer rejected because the target became terminal")
			}
			e.turns[index].AcceptedMessageIDs = append(e.turns[index].AcceptedMessageIDs, delivery.AgentRun.ID)
			return e.turns[index].ID, nil
		}
	}
	return "", errors.New("missing steer target")
}

func newFakeExternals() *fakeExternals {
	return &fakeExternals{outcomes: map[int64]string{}, secondClaim: make(chan struct{})}
}
func (f *fakeExternals) submittedSequences() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.submitted...)
}
func (f *fakeExternals) effect(action Action) (Receipt, error) {
	f.mu.Lock()
	f.effects = append(f.effects, action.Kind)
	f.mu.Unlock()
	return Receipt{ExternalID: "native-" + string(action.Kind)}, nil
}
func (f *fakeExternals) SandboxCreate(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) RepositoryClone(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) RouteCreate(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) AgentInitialTurn(_ context.Context, _ Job, delivery Delivery) (string, NativeTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.turns) == 0 {
		f.submitted = append(f.submitted, delivery.Message.Sequence)
		turn := NativeTurn{ID: "turn-" + delivery.Message.ID, Status: "running"}
		f.turns = append(f.turns, turn)
	}
	return "session-1", f.turns[0], nil
}
func (f *fakeExternals) AgentInitialTurns(_ context.Context, _ Job) (string, []NativeTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initialSessionID, append([]NativeTurn(nil), f.turns...), nil
}
func (f *fakeExternals) AgentTurns(_ context.Context, _ Job, _ string) ([]NativeTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]NativeTurn(nil), f.turns...), nil
}
func (f *fakeExternals) AgentSubmit(_ context.Context, _ Job, delivery Delivery) (NativeTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitted = append(f.submitted, delivery.Message.Sequence)
	turn := NativeTurn{ID: "turn-" + delivery.Message.ID, Status: "running"}
	f.turns = append(f.turns, turn)
	return turn, f.submitError
}
func (f *fakeExternals) AgentWait(_ context.Context, _ Job, _ string, turnID string) (NativeTurn, error) {
	f.mu.Lock()
	sequence := f.submitted[len(f.submitted)-1]
	if sequence == 1 && f.firstActive != nil {
		f.activeOnce.Do(func() { close(f.firstActive) })
	}
	block := f.blockFirst
	f.mu.Unlock()
	if sequence == 1 && block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.outcomes[sequence]
	if status == "" {
		status = "completed"
	}
	for i := range f.turns {
		if f.turns[i].ID == turnID {
			f.turns[i].Status = status
		}
	}
	return NativeTurn{ID: turnID, Status: status}, nil
}
func (f *fakeExternals) RouteRevoke(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) SandboxDelete(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}

func testJob() Job {
	return Job{ID: "job-0123456789abcdef", Goal: "goal", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/proof", Model: "gpt-5.6-sol", ReasoningEffort: "high", AdmissionOpen: true, CleanupState: CleanupPending}
}
