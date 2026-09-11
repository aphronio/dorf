package postgres_test

import (
	"context"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestSkillRefreshSurvivesSteeringQueueFailureAndRestart(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "skill-refresh")
	first, err := codingDelivery(ctx, store, job.ID)
	if err != nil || first == nil {
		t.Fatalf("initial=%+v err=%v", first, err)
	}
	if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, "turn-first", "running"); err != nil {
		t.Fatal(err)
	}
	admit := func(key string, intent core.MessageDeliveryIntent, refresh bool) core.MessageAdmissionResult {
		t.Helper()
		result, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: key, Input: key, Intent: intent, RefreshSkills: refresh})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	execution := func(message core.MessageAdmissionResult, refresh bool) core.AgentMessageExecution {
		t.Helper()
		got, err := store.AgentMessageExecution(ctx, message.Message.ID)
		if err != nil || got.RefreshSkills != refresh {
			t.Fatalf("execution refresh=%t want=%t err=%v", got.RefreshSkills, refresh, err)
		}
		return got
	}
	queued := admit("queued-before-refresh", core.MessageFollow, false)
	steer := admit("refresh-active", core.MessageAuto, true)
	if steer.Message.Intent != core.MessageSteer || steer.Message.TargetTurnID != "turn-first" {
		t.Fatalf("steer=%+v", steer.Message)
	}
	steerExecution := execution(steer, false)
	if err := store.PrepareAgentRun(ctx, steerExecution.AgentRun.ID, "codex", "turn-first"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindSteer(ctx, steerExecution.AgentRun.ID, "turn-first", "running"); err != nil {
		t.Fatal(err)
	}
	if selected, err := store.AgentMessage(ctx, job.ID); err != nil || selected == nil || selected.MessageID != first.Message.ID {
		t.Fatalf("active selection=%+v err=%v", selected, err)
	}
	if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, "turn-first", "completed"); err != nil {
		t.Fatal(err)
	}
	store = postgres.Store{DB: store.DB}
	if selected, err := store.AgentMessage(ctx, job.ID); err != nil || selected == nil || selected.MessageID != queued.Message.ID {
		t.Fatalf("queued selection=%+v err=%v", selected, err)
	}
	inherited := execution(queued, true)
	if inherited.Message.RefreshSkills {
		t.Fatal("inherited refresh changed original request")
	}
	replay := admit("queued-before-refresh", core.MessageFollow, false)
	if replay.Created || replay.Message.RefreshSkills {
		t.Fatalf("original replay=%+v", replay)
	}
	if err := store.PrepareAgentRun(ctx, inherited.AgentRun.ID, "codex", "turn-first"); err != nil {
		t.Fatal(err)
	}
	if err := store.FailAgentRun(ctx, inherited.AgentRun.ID, "skill refresh rejected before submission"); err != nil {
		t.Fatal(err)
	}
	next := admit("retry-after-failure", core.MessageAuto, false)
	if next.Message.Intent != core.MessageFollow {
		t.Fatalf("idle auto=%+v", next.Message)
	}
	if selected, err := store.AgentMessage(ctx, job.ID); err != nil || selected == nil || selected.MessageID != next.Message.ID {
		t.Fatalf("retry selection=%+v err=%v", selected, err)
	}
	retried := execution(next, true)
	if err := store.PrepareAgentRun(ctx, retried.AgentRun.ID, "codex", "turn-first"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, retried.AgentRun.ID, "codex", threadID, "turn-refreshed", "completed"); err != nil {
		t.Fatal(err)
	}
	ordinary := admit("ordinary-after-refresh", core.MessageAuto, false)
	execution(ordinary, false)
	otherJob, _ := prepareTransportIntegrationJob(t, store, "skill-refresh-other-job")
	other, err := codingDelivery(ctx, store, otherJob.ID)
	if err != nil || other == nil {
		t.Fatalf("other=%+v err=%v", other, err)
	}
	otherExecution, err := store.AgentMessageExecution(ctx, other.Message.ID)
	if err != nil || otherExecution.RefreshSkills {
		t.Fatalf("other refresh=%t err=%v", otherExecution.RefreshSkills, err)
	}
}

func TestFailedFollowRetainsSkillRefreshForNextFollow(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "failed-follow-refresh")
	first, err := codingDelivery(ctx, store, job.ID)
	if err != nil || first == nil {
		t.Fatalf("initial=%+v err=%v", first, err)
	}
	if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, first.AgentRun.ID, "codex", threadID, "initial-turn", "completed"); err != nil {
		t.Fatal(err)
	}
	flagged, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "refresh", Input: "refresh", RefreshSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareAgentRun(ctx, core.AgentRunID(flagged.Message.ID), "codex", "initial-turn"); err != nil {
		t.Fatal(err)
	}
	if err := store.FailAgentRun(ctx, core.AgentRunID(flagged.Message.ID), "refresh rejected"); err != nil {
		t.Fatal(err)
	}
	next, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: "human", FromID: "retry", Input: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := store.AgentMessageExecution(ctx, next.Message.ID)
	if err != nil || !execution.RefreshSkills || execution.Message.RefreshSkills {
		t.Fatalf("next=%+v err=%v", execution, err)
	}
}
