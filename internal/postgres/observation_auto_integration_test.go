package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestAutoObservationAdoptsNativeFollowAfterLostAcknowledgement(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx := context.Background()
	job, threadID := prepareTransportIntegrationJob(t, store, "auto-steer-error-terminal-follow")
	target, err := codingDelivery(ctx, store, job.ID)
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
	automatic, err := store.AdmitCodingMessage(ctx, core.MessageAdmission{
		JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
		FromID: "steer-error-terminal-auto", Input: "preserve after uncertain acknowledgement", Intent: core.MessageAuto, Observation: true,
	})
	if err != nil || !automatic.Created || automatic.Message.Intent != core.MessageSteer {
		t.Fatalf("automatic=%#v err=%v", automatic, err)
	}
	externals := &integrationExternals{
		turnStatus:      "running",
		turns:           []core.HarnessTurn{{ID: targetTurnID, Status: "running"}},
		steerErr:        errors.New("steer acknowledgement lost"),
		terminalOnSteer: true,
		startOnSteer:    true,
	}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
		WithAgentExecution(resultBoundaryAgentExecution{externals: externals})
	taskName := "dorf-steer-error-terminal-auto-follow-proof-v1"
	client.MustRegister(absurd.Task(taskName, func(taskCtx context.Context, _ core.JobTaskParams) (core.TaskResultV1, error) {
		if _, err := execution.ReconcileJobAgent(taskCtx, job.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		if _, err := execution.ReconcileJobAgent(taskCtx, job.ID); err != nil {
			return core.TaskResultV1{}, err
		}
		adopted, err := store.AgentMessageExecution(taskCtx, automatic.Message.ID)
		if err != nil || adopted.Message.Intent != core.MessageFollow || adopted.AgentRun.TurnID != "event-follow" || adopted.AgentRun.State != core.AgentRunActive {
			return core.TaskResultV1{}, fmt.Errorf("accepted event follow was not adopted: %+v %v", adopted, err)
		}
		return core.TaskResultV1{JobID: job.ID, Outcome: "terminal-auto-follow-requeued"}, nil
	}))
	spawned, err := client.Spawn(ctx, taskName, core.JobTaskParams{JobID: job.ID}, absurd.SpawnOptions{IdempotencyKey: taskName + ":" + job.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AttachJobTask(ctx, job.ID, "", spawned.TaskID, taskName); err != nil {
		t.Fatal(err)
	}
	if err := client.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "steer-error-terminal-auto-follow", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AwaitTaskResult(ctx, client.QueueName(), spawned.TaskID); err != nil {
		t.Fatal(err)
	}
	if submitted := externals.submittedSequences(); !reflect.DeepEqual(submitted, []int64{automatic.Message.Sequence}) {
		t.Fatalf("submitted sequences=%v", submitted)
	}
}
