package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type ExecutionStore interface {
	Job(context.Context, string) (Job, error)
	JobTasks(context.Context, string) ([]JobTask, error)
	Sandboxes(context.Context, string) ([]Sandbox, error)
	Deliveries(context.Context, string) ([]Delivery, error)
	AgentMessageExecution(context.Context, string) (AgentMessageExecution, error)
	InterruptAgentRun(context.Context, string, string) error
	WithJobFence(context.Context, string, func() error) error
	AuthorizeSandboxAction(context.Context, string, string, string) (SandboxActionAuthorization, error)
	RecordSandboxActionSuccess(context.Context, string) error
	PrepareAgentRun(context.Context, string, string, string) error
	BindAgentRun(context.Context, string, string, string, string, string) error
	BindSteer(context.Context, string, string, string) error
	FailAgentRun(context.Context, string, string) error
	UncertainAgentRun(context.Context, string, string) error
	AgentRunAttention(context.Context, string, string) error
	UnsettledAgentMessages(context.Context, string) ([]AgentMessageWork, error)
	CompleteCleanup(context.Context, string, string) error
	SetCleanupAttention(context.Context, string, string) error
}

type Externals interface {
	Harness() string
	SandboxCreate(context.Context, Job, Sandbox) error
	RouteCreate(context.Context, Job, Sandbox, Route) error
	AgentInitialTurn(context.Context, Job, Delivery, string) (HarnessBinding, error)
	AgentInitialTurns(context.Context, Job, string) (HarnessHistory, error)
	AgentTurns(context.Context, Job, string, string) (HarnessHistory, error)
	AgentSubmit(context.Context, Job, Delivery, string) (HarnessBinding, error)
	AgentSteer(context.Context, Job, Delivery) (string, error)
	RouteRevoke(context.Context, Job, Sandbox, Route) error
	SandboxDelete(context.Context, Job, Sandbox) error
}

type FaultBarrier interface {
	Reach(context.Context, string, Delivery) error
	ReachWorkflow(context.Context, string, string, string) error
}

// AgentHarnessStrategy is a cohesive adapter selected once from authoritative
// facts. Core owns its prepare/recover/submit/observe/bind state machine; the
// strategy owns only workflow-specific Harness translation and validation.
type AgentHarnessStrategy interface {
	Harness() string
	SubmitNew(context.Context, AgentRun) (HarnessBinding, error)
	Recover(context.Context, AgentRun) (HarnessBinding, error)
	History(context.Context, AgentRun) (HarnessHistory, error)
}

type AgentStrategyResolver interface {
	ResolveAgentPrompt(context.Context, AgentMessageExecution) (string, error)
	ResolveAgentHarnessStrategy(context.Context, AgentMessageExecution) (AgentHarnessStrategy, error)
	ResolveCleanupAgentStrategy(context.Context, AgentMessageExecution) (AgentHarnessStrategy, error)
}

const (
	BarrierBeforeSubmit          = "before-submit"
	BarrierAfterSubmitBeforeBind = "after-submit-before-bind"
	BarrierHarnessActive         = "harness-active"
	BarrierPushAccepted          = "push-accepted-before-record"
	BarrierPullRequestAccepted   = "pull-request-accepted-before-record"
	BarrierSandboxCreated        = "sandbox-created-before-record"
	BarrierRouteRevoked          = "route-revoked-before-record"
	BarrierSandboxDeleted        = "sandbox-deleted-before-record"
)

type ExecutionService struct {
	store      ExecutionStore
	externals  Externals
	barrier    FaultBarrier
	blobs      blob.Store
	claimCheck func(context.Context) error
	strategies AgentStrategyResolver
}

func (s ExecutionService) WithAgentStrategies(strategies AgentStrategyResolver) ExecutionService {
	s.strategies = strategies
	return s
}

func NewExecutionService(store ExecutionStore, externals Externals, records blob.Store, barrier FaultBarrier, claimCheck func(context.Context) error) ExecutionService {
	return ExecutionService{
		store:      store,
		externals:  externals,
		barrier:    barrier,
		blobs:      records,
		claimCheck: claimCheck,
	}
}

func (s ExecutionService) BlobStore() blob.Store { return s.blobs }

func (s ExecutionService) requireClaim(ctx context.Context) error {
	if s.claimCheck == nil {
		return errors.New("durable executor claim check is not configured")
	}
	return s.claimCheck(ctx)
}

func (s ExecutionService) recordAgentRun(ctx context.Context, record func() error) error {
	return claimBeforeAgentRunRecord(ctx, s.requireClaim, record)
}

func claimBeforeAgentRunRecord(ctx context.Context, claimCheck func(context.Context) error, record func() error) error {
	if err := claimCheck(ctx); err != nil {
		return err
	}
	return record()
}

func (s ExecutionService) agentRunAttention(ctx context.Context, runID, detail string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.AgentRunAttention(ctx, runID, detail) })
}

func (s ExecutionService) prepareAgentRun(ctx context.Context, runID, harness, baseline string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.PrepareAgentRun(ctx, runID, harness, baseline) })
}

func (s ExecutionService) bindAgentRun(ctx context.Context, runID, harness, threadID, turnID, outcome string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.BindAgentRun(ctx, runID, harness, threadID, turnID, outcome) })
}

func (s ExecutionService) bindSteer(ctx context.Context, runID, turnID, outcome string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.BindSteer(ctx, runID, turnID, outcome) })
}

func (s ExecutionService) failAgentRun(ctx context.Context, runID, reason string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.FailAgentRun(ctx, runID, reason) })
}

func (s ExecutionService) uncertainAgentRun(ctx context.Context, runID, reason string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.UncertainAgentRun(ctx, runID, reason) })
}

func attentionNeeded(err error) bool {
	var attention interface{ AttentionNeeded() bool }
	return errors.As(err, &attention) && attention.AttentionNeeded()
}

func (s ExecutionService) reachWorkflow(ctx context.Context, point, jobID, identity string) error {
	if s.barrier == nil {
		return nil
	}
	return s.barrier.ReachWorkflow(ctx, point, jobID, identity)
}

// ReconcileAgentMessage performs one bounded execution/recovery cycle from a
// stable Job, Message, and Sandbox identities. Every execution fact is loaded
// while the Job effect fence is held, independently of the expiring Absurd
// claim.
func (s ExecutionService) ReconcileAgentMessage(ctx context.Context, jobID, messageID, sandboxID string) (MessageResult, error) {
	if jobID == "" || messageID == "" || sandboxID == "" {
		return MessageResult{}, fmt.Errorf("Agent reconciliation requires exact Job, Message, and Sandbox identities")
	}
	result := MessageResult{MessageID: messageID}
	err := s.store.WithJobFence(ctx, jobID, func() error {
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		authoritative, err := s.store.AgentMessageExecution(ctx, messageID)
		if err != nil {
			return err
		}
		if authoritative.Job.ID != jobID || authoritative.Message.ID != messageID ||
			authoritative.Sandbox.ID != sandboxID || authoritative.AgentRun.SandboxID != sandboxID {
			return fmt.Errorf("Message %s does not belong to the exact bound Job Sandbox", messageID)
		}
		if !authoritative.Job.AdmissionOpen || authoritative.Job.CleanupState != CleanupPending || authoritative.Job.CurrentTaskID != task.TaskID() {
			return fmt.Errorf("task %s cannot reconcile Message %s outside the exact current open Job attachment", task.TaskID(), messageID)
		}
		attachments, err := s.store.JobTasks(ctx, authoritative.Job.ID)
		if err != nil {
			return err
		}
		attached := false
		for _, attachment := range attachments {
			if attachment.TaskID == task.TaskID() && attachment.TaskName == task.TaskName() {
				attached = true
				break
			}
		}
		if !attached {
			return fmt.Errorf("task %s has no exact durable Job attachment for Message %s", task.TaskID(), messageID)
		}
		if s.strategies == nil {
			return fmt.Errorf("Agent strategy resolution is not configured")
		}
		runner, err := s.strategies.ResolveAgentHarnessStrategy(ctx, authoritative)
		if err != nil {
			return err
		}
		delivery := Delivery{Message: authoritative.Message, AgentRun: authoritative.AgentRun}
		run := authoritative.AgentRun
		if runner != nil {
			if run.State == AgentRunFailed || run.State == AgentRunInterrupted {
				result.Outcome = run.TurnOutcome
				if result.Outcome == "" {
					result.Outcome = string(run.State)
				}
				return nil
			}
			turn, err := s.executeAgentStrategy(ctx, delivery, runner)
			if err != nil {
				return err
			}
			if turn.Terminal() {
				result.Outcome, result.Output = turn.Status, turn.Output
			}
			return nil
		}
		input, err := s.strategies.ResolveAgentPrompt(ctx, authoritative)
		if err != nil {
			return err
		}
		if input == "" {
			return fmt.Errorf("Message %s strategy returned empty agent input", messageID)
		}
		// Prompt eligibility may durably adopt an early queued follow onto the
		// now-authoritative prior Thread. Discard the pre-resolution aggregate so
		// bound submission can never fall back to initial recovery.
		authoritative, err = s.store.AgentMessageExecution(ctx, messageID)
		if err != nil {
			return err
		}
		if authoritative.Job.ID != jobID || authoritative.Sandbox.ID != sandboxID || authoritative.AgentRun.SandboxID != sandboxID {
			return fmt.Errorf("Message %s changed authoritative Job or Sandbox during strategy resolution", messageID)
		}
		delivery = Delivery{Message: authoritative.Message, AgentRun: authoritative.AgentRun}
		run = authoritative.AgentRun
		if run.State == AgentRunCompleted {
			turn, err := s.observeAgentRunTurn(ctx, authoritative.Job, run, run.Role)
			if err != nil {
				return err
			}
			if turn.Terminal() {
				result.Outcome, result.Output = turn.Status, turn.Output
			}
			return nil
		}
		if run.State == AgentRunFailed || run.State == AgentRunInterrupted {
			result.Outcome = run.TurnOutcome
			if result.Outcome == "" {
				result.Outcome = string(run.State)
			}
			return nil
		}
		if run.State == AgentRunActive {
			turn, err := s.observeAgentRunTurn(ctx, authoritative.Job, run, run.Role)
			if err != nil {
				return err
			}
			if turn.Terminal() {
				result.Outcome, result.Output = turn.Status, turn.Output
			}
			return nil
		}
		if err := s.deliver(ctx, authoritative.Job, delivery, input); err != nil {
			return err
		}
		settled, err := s.store.AgentMessageExecution(ctx, messageID)
		if err != nil {
			return err
		}
		if settled.AgentRun.State == AgentRunCompleted {
			turn, err := s.observeAgentRunTurn(ctx, settled.Job, settled.AgentRun, settled.AgentRun.Role)
			if err != nil {
				return err
			}
			if turn.Terminal() {
				result.Outcome, result.Output = turn.Status, turn.Output
			}
		} else if settled.AgentRun.State == AgentRunFailed || settled.AgentRun.State == AgentRunInterrupted {
			result.Outcome = settled.AgentRun.TurnOutcome
			if result.Outcome == "" {
				result.Outcome = string(settled.AgentRun.State)
			}
		}
		return nil
	})
	return result, err
}

func (s ExecutionService) executeAgentStrategy(ctx context.Context, delivery Delivery, strategy AgentHarnessStrategy) (HarnessTurn, error) {
	contract := agentRunContract{
		store: s.store, reachBarrier: s.reach, delivery: delivery, run: delivery.AgentRun,
		harness: strategy.Harness(), label: "harness",
		submitNew: strategy.SubmitNew, recover: strategy.Recover, history: strategy.History,
		beforeRecord: s.requireClaim,
		onReadError: func(ctx context.Context, runID string, err error) {
			_ = s.agentRunAttention(ctx, runID, "Harness history is unavailable: "+err.Error())
		},
		onRecoverError: func(ctx context.Context, run AgentRun, err error) error {
			if persistErr := s.agentRunAttention(ctx, run.ID, "Harness recovery is unavailable: "+err.Error()); persistErr != nil {
				return persistErr
			}
			return err
		},
		onSubmitError: func(ctx context.Context, run AgentRun, _ HarnessBinding, err error) (HarnessTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if persistErr := s.failAgentRun(ctx, run.ID, err.Error()); persistErr != nil {
					return HarnessTurn{}, persistErr
				}
				return HarnessTurn{Status: "failed"}, nil
			}
			if persistErr := s.uncertainAgentRun(ctx, run.ID, err.Error()); persistErr != nil {
				return HarnessTurn{}, persistErr
			}
			return HarnessTurn{}, err
		},
	}
	return contract.execute(ctx)
}

func (s ExecutionService) deliver(ctx context.Context, job Job, delivery Delivery, input string) error {
	if delivery.Message.Intent == MessageSteer && (delivery.AgentRun.TurnID == "" || delivery.AgentRun.TurnID == delivery.Message.TargetTurnID) {
		return s.deliverSteer(ctx, job, delivery, input)
	}
	return s.deliverAgentRun(ctx, job, delivery, input)
}

func (s ExecutionService) deliverAgentRun(ctx context.Context, job Job, delivery Delivery, input string) error {
	run := delivery.AgentRun
	contract := agentRunContract{
		store:               s.store,
		reachBarrier:        s.reach,
		delivery:            delivery,
		run:                 run,
		harness:             s.externals.Harness(),
		label:               "harness",
		bindUnsupportedTurn: true,
		submitNew: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			delivery.AgentRun = run
			return s.externals.AgentInitialTurn(ctx, job, delivery, input)
		},
		recover: func(ctx context.Context, _ AgentRun) (HarnessBinding, error) {
			history, err := s.externals.AgentInitialTurns(ctx, job, run.SandboxID)
			if err != nil || history.ThreadID == "" || len(history.Turns) == 0 {
				return HarnessBinding{}, err
			}
			return HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: history.Turns[len(history.Turns)-1]}, nil
		},
		submitBound: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			delivery.AgentRun = run
			return s.externals.AgentSubmit(ctx, job, delivery, input)
		},
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return s.externals.AgentTurns(ctx, job, run.SandboxID, run.ThreadID)
		},
		beforeRecord: s.requireClaim,
		onReadError: func(ctx context.Context, runID string, err error) {
			_ = s.agentRunAttention(ctx, runID, "harness thread or submitted turn is currently unavailable: "+err.Error())
		},
		onSubmitError: func(ctx context.Context, run AgentRun, _ HarnessBinding, err error) (HarnessTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if failErr := s.failAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return HarnessTurn{}, failErr
				}
			}
			if attentionNeeded(err) {
				if uncertainErr := s.uncertainAgentRun(ctx, run.ID, err.Error()); uncertainErr != nil {
					return HarnessTurn{}, uncertainErr
				}
			}
			return HarnessTurn{}, err
		},
	}
	_, err := contract.execute(ctx)
	return err
}

func (s ExecutionService) observeAgentRunTurn(ctx context.Context, job Job, run AgentRun, role string) (HarnessTurn, error) {
	if run.JobID != job.ID || run.Role != role || run.ThreadID == "" || run.TurnID == "" {
		return HarnessTurn{}, fmt.Errorf("AgentRun %s is not an exact bound %s Turn for Job %s", run.ID, role, job.ID)
	}
	contract := agentRunContract{
		store:   s.store,
		run:     run,
		harness: s.externals.Harness(),
		label:   "harness",
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return s.externals.AgentTurns(ctx, job, run.SandboxID, run.ThreadID)
		},
		beforeRecord: s.requireClaim,
		onReadError: func(ctx context.Context, runID string, err error) {
			_ = s.agentRunAttention(ctx, runID, "harness thread or submitted turn is currently unavailable: "+err.Error())
		},
	}
	turn, err := contract.execute(ctx)
	if err != nil {
		return HarnessTurn{}, err
	}
	return turn, nil
}

func (s ExecutionService) deliverSteer(ctx context.Context, job Job, delivery Delivery, input string) error {
	run := delivery.AgentRun
	history, err := s.externals.AgentTurns(ctx, job, run.SandboxID, run.ThreadID)
	if err != nil {
		_ = s.agentRunAttention(ctx, run.ID, "harness thread history is currently unavailable: "+err.Error())
		return err
	}
	turns := history.Turns
	reconciliation := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, turns)
	if reconciliation.Classification == "completed" {
		return s.bindSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciliation.Turn.Status)
	}
	if reconciliation.Classification == "target-terminal" {
		if !run.BaselineRecorded && turns[len(turns)-1].ID != delivery.Message.TargetTurnID {
			return s.uncertainAgentRun(ctx, run.ID, "harness turns appeared after the terminal steer target before a fallback baseline was recorded")
		}
		return s.deliverAgentRun(ctx, job, delivery, input)
	}
	if reconciliation.Classification == "uncertain" {
		return s.uncertainAgentRun(ctx, run.ID, reconciliation.Reason)
	}
	if !run.BaselineRecorded {
		if err := s.prepareAgentRun(ctx, run.ID, run.Harness, delivery.Message.TargetTurnID); err != nil {
			return err
		}
		delivery.AgentRun.BaselineRecorded = true
		delivery.AgentRun.BaselineTurnID = delivery.Message.TargetTurnID
	}
	if err := s.reach(ctx, BarrierBeforeSubmit, delivery); err != nil {
		return err
	}
	acceptedTurnID, err := s.externals.AgentSteer(ctx, job, delivery)
	if err != nil {
		observedHistory, inspectErr := s.externals.AgentTurns(ctx, job, run.SandboxID, run.ThreadID)
		if inspectErr != nil {
			reason := "harness steer acknowledgement is genuinely uncertain: " + err.Error() + "; history inspection failed: " + inspectErr.Error()
			return s.uncertainAgentRun(ctx, run.ID, reason)
		}
		observed := observedHistory.Turns
		reconciled := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, observed)
		if reconciled.Classification == "completed" {
			return s.bindSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciled.Turn.Status)
		}
		if reconciled.Classification == "target-terminal" {
			if !delivery.AgentRun.BaselineRecorded && observed[len(observed)-1].ID != delivery.Message.TargetTurnID {
				return s.uncertainAgentRun(ctx, run.ID, "harness turns appeared after the terminal steer target before a fallback baseline was recorded")
			}
			return s.deliverAgentRun(ctx, job, delivery, input)
		}
		if reconciled.Classification == "uncertain" {
			return s.uncertainAgentRun(ctx, run.ID, reconciled.Reason)
		}
		return err
	}
	if acceptedTurnID != delivery.Message.TargetTurnID {
		return s.uncertainAgentRun(ctx, run.ID, "harness steer acknowledgement named a different active turn")
	}
	if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
		return err
	}
	return s.bindSteer(ctx, run.ID, acceptedTurnID, reconciliation.Turn.Status)
}

func (s ExecutionService) reach(ctx context.Context, point string, delivery Delivery) error {
	if s.barrier == nil {
		return nil
	}
	return s.barrier.Reach(ctx, point, delivery)
}

func terminalHarness(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func activeHarness(status string) bool {
	// "inProgress" is the app-server thread/read spelling. "running" is
	// Dorf's local status immediately after turn/start acceptance.
	return status == "running" || status == "inProgress"
}

// PrepareCleanup reconciles harness ownership and returns the exact Sandboxes
// whose cleanup Actions the workflow must execute under their own stable
// Action Steps.
func (s ExecutionService) PrepareCleanup(ctx context.Context, jobID string) (Job, []Sandbox, error) {
	var job Job
	var sandboxes []Sandbox
	err := s.store.WithJobFence(ctx, jobID, func() error {
		if err := s.requireCleanupTask(ctx, jobID); err != nil {
			return err
		}
		var err error
		job, err = s.store.Job(ctx, jobID)
		if err != nil {
			return err
		}
		if job.AdmissionOpen {
			return fmt.Errorf("cleanup recovery requires closed admission and a stopped ordinary run")
		}
		if job.CleanupState == CleanupComplete {
			return nil
		}
		if job.CleanupState != CleanupScheduled {
			return fmt.Errorf("cleanup recovery requires a durably scheduled cleanup task")
		}
		if err := s.cleanupStep(ctx, job.ID, "reconciling every unsettled harness mutation", func() error {
			return s.reconcileHarnessMutations(ctx, job)
		}); err != nil {
			return err
		}
		deliveries, err := s.store.Deliveries(ctx, job.ID)
		if err != nil {
			return err
		}
		for _, delivery := range deliveries {
			run := delivery.AgentRun
			settled := run.State == AgentRunCompleted || run.State == AgentRunFailed || run.State == AgentRunInterrupted
			if settled {
				continue
			}
			if run.State != AgentRunPending || run.BaselineRecorded {
				return cleanupBlocked(delivery, "a possibly accepted harness mutation remains")
			}
			if err := s.requireClaim(ctx); err != nil {
				return err
			}
			if err := s.store.InterruptAgentRun(ctx, run.ID, "admission closed before any harness mutation; Job resources are being reclaimed"); err != nil {
				return err
			}
		}
		sandboxes, err = s.store.Sandboxes(ctx, job.ID)
		return err
	})
	return job, sandboxes, err
}

func (s ExecutionService) requireCleanupTask(ctx context.Context, jobID string) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	task, ok := absurd.TaskFromContext(ctx)
	if !ok {
		return absurd.ErrNoTaskContext
	}
	job, err := s.store.Job(ctx, jobID)
	if err != nil {
		return err
	}
	if task.TaskName() != CleanupTaskName || job.CurrentTaskID != task.TaskID() {
		return fmt.Errorf("task %s is not the exact current cleanup attachment for Job %s", task.TaskID(), jobID)
	}
	attachments, err := s.store.JobTasks(ctx, jobID)
	if err != nil {
		return err
	}
	for _, attachment := range attachments {
		if attachment.TaskID == task.TaskID() && attachment.TaskName == CleanupTaskName {
			return nil
		}
	}
	return fmt.Errorf("task %s has no exact durable cleanup attachment for Job %s", task.TaskID(), jobID)
}

func (s ExecutionService) cleanupStep(ctx context.Context, jobID, detail string, fn func() error) error {
	if err := s.store.SetCleanupAttention(ctx, jobID, detail); err != nil {
		return err
	}
	if err := fn(); err != nil {
		var active cleanupStillActive
		if errors.As(err, &active) {
			return err
		}
		_ = s.store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
		return err
	}
	return nil
}

func (s ExecutionService) reconcileHarnessMutations(ctx context.Context, job Job) error {
	messages, err := s.store.UnsettledAgentMessages(ctx, job.ID)
	if err != nil {
		return err
	}
	var settlementErrors []error
	for _, message := range messages {
		execution, err := s.store.AgentMessageExecution(ctx, message.MessageID)
		if err == nil && (execution.Job.ID != job.ID || execution.Sandbox.ID != message.SandboxID) {
			err = fmt.Errorf("unsettled Message %s no longer matches its authoritative Job and Sandbox", message.MessageID)
		}
		if err == nil {
			err = s.reconcileCleanupMessage(ctx, execution)
		}
		if err != nil {
			settlementErrors = append(settlementErrors, err)
		}
	}
	return errors.Join(settlementErrors...)
}

func (s ExecutionService) reconcileCleanupMessage(ctx context.Context, execution AgentMessageExecution) error {
	delivery := Delivery{Message: execution.Message, AgentRun: execution.AgentRun}
	run := execution.AgentRun
	var runner AgentHarnessStrategy
	if s.strategies != nil {
		var err error
		runner, err = s.strategies.ResolveCleanupAgentStrategy(ctx, execution)
		if err != nil {
			return cleanupBlocked(delivery, "resolve exact Harness recovery strategy: "+err.Error())
		}
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	history := func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
		if runner != nil {
			return runner.History(ctx, run)
		}
		return s.externals.AgentTurns(ctx, execution.Job, run.SandboxID, run.ThreadID)
	}
	var inspected *HarnessHistory
	if delivery.Message.Intent == MessageSteer && (run.TurnID == "" || run.TurnID == delivery.Message.TargetTurnID) {
		observed, err := history(ctx, run)
		if err != nil {
			return s.retainCleanupMutation(ctx, delivery, "bound Harness history is unavailable: "+err.Error())
		}
		steer := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, observed.Turns)
		switch steer.Classification {
		case "completed":
			if err := s.bindSteer(ctx, run.ID, delivery.Message.TargetTurnID, steer.Turn.Status); err != nil {
				return err
			}
			if !terminalHarness(steer.Turn.Status) {
				return cleanupStillActive{MessageID: delivery.Message.ID, Reason: "accepted steer remains active"}
			}
			return nil
		case "no-submit":
			return s.failAgentRun(ctx, run.ID, "cleanup closed steer after exact Harness history proved it was not accepted")
		case "target-terminal":
			// A terminal target may have raced into ordinary fallback before cleanup.
			// Observe the durable baseline below, but never create that fallback here.
			inspected = &observed
		case "uncertain":
			return s.retainCleanupMutation(ctx, delivery, steer.Reason)
		default:
			return s.retainCleanupMutation(ctx, delivery, "unsupported steer recovery classification")
		}
	}
	contractHistory := history
	if inspected != nil {
		contractHistory = func(context.Context, AgentRun) (HarnessHistory, error) { return *inspected, nil }
	}
	recover := func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
		if runner != nil {
			return runner.Recover(ctx, run)
		}
		observed, err := s.externals.AgentInitialTurns(ctx, execution.Job, run.SandboxID)
		if err != nil {
			return HarnessBinding{}, err
		}
		if len(observed.Turns) != 1 {
			return HarnessBinding{}, fmt.Errorf("initial Harness recovery returned %d turns, not one exact accepted mutation", len(observed.Turns))
		}
		return HarnessBinding{Harness: observed.Harness, ThreadID: observed.ThreadID, Turn: observed.Turns[0]}, nil
	}
	harness := s.externals.Harness()
	if runner != nil {
		harness = runner.Harness()
	}
	contract := agentRunContract{
		store: s.store, delivery: delivery, run: run, harness: harness, label: "cleanup harness",
		recover: recover, history: contractHistory, beforeRecord: s.requireClaim, settleOnly: true,
		onNoSubmit: func(ctx context.Context, run AgentRun) (HarnessTurn, error) {
			if err := s.failAgentRun(ctx, run.ID, "cleanup closed delivery after exact Harness history proved no turn was submitted"); err != nil {
				return HarnessTurn{}, err
			}
			return HarnessTurn{Status: "failed"}, nil
		},
	}
	turn, err := contract.execute(ctx)
	if err != nil {
		return s.retainCleanupMutation(ctx, delivery, err.Error())
	}
	if activeHarness(turn.Status) {
		return cleanupStillActive{MessageID: delivery.Message.ID, Reason: "accepted Harness mutation remains active"}
	}
	return nil
}

func (s ExecutionService) retainCleanupMutation(ctx context.Context, delivery Delivery, reason string) error {
	if err := s.agentRunAttention(ctx, delivery.AgentRun.ID, reason); err != nil {
		return err
	}
	return cleanupBlocked(delivery, reason)
}

func cleanupBlocked(delivery Delivery, reason string) error {
	if reason == "" {
		reason = string(delivery.AgentRun.State)
	}
	return fmt.Errorf("cleanup retained Sandbox and route: message sequence %d is not safely settled (%s)", delivery.Message.Sequence, reason)
}

type cleanupStillActive struct {
	MessageID string
	Reason    string
}

func (e cleanupStillActive) Error() string {
	return fmt.Sprintf("cleanup is waiting for active Message %s: %s", e.MessageID, e.Reason)
}

// ExecuteSandboxAction reconciles one authoritative persisted mutation. The
// caller supplies only the Job and Action identities used by durable custody.
func (s ExecutionService) ExecuteSandboxAction(ctx context.Context, jobID, actionID string) error {
	return s.executeSandboxAction(ctx, jobID, actionID, "", func(ctx context.Context, authorized SandboxActionAuthorization) error {
		switch authorized.Action.Kind {
		case ActionSandboxCreate:
			return s.externals.SandboxCreate(ctx, authorized.Job, authorized.Sandbox)
		case ActionRouteCreate:
			return s.externals.RouteCreate(ctx, authorized.Job, authorized.Sandbox, RouteForSandbox(authorized.Sandbox))
		case ActionRouteRevoke:
			return s.externals.RouteRevoke(ctx, authorized.Job, authorized.Sandbox, RouteForSandbox(authorized.Sandbox))
		case ActionSandboxDelete:
			return s.externals.SandboxDelete(ctx, authorized.Job, authorized.Sandbox)
		default:
			return fmt.Errorf("unsupported Sandbox Action kind %q", authorized.Action.Kind)
		}
	})
}

// ExecuteSandboxActionEffect gives workflow-owned Sandbox mutations the same
// Core custody, Job fencing, exact task authority, and durable Action receipt
// as provider-owned actions without moving their semantics into Core.
func (s ExecutionService) ExecuteSandboxActionEffect(ctx context.Context, jobID, actionID string, kind ActionKind, effect SandboxActionEffect) error {
	if effect == nil {
		return fmt.Errorf("Sandbox Action %s has no workflow-owned effect", actionID)
	}
	return s.executeSandboxAction(ctx, jobID, actionID, kind, func(ctx context.Context, authorized SandboxActionAuthorization) error {
		return effect(ctx, authorized.Job, authorized.Sandbox)
	})
}

func (s ExecutionService) executeSandboxAction(ctx context.Context, jobID, actionID string, expectedKind ActionKind, effect func(context.Context, SandboxActionAuthorization) error) error {
	if jobID == "" || actionID == "" {
		return fmt.Errorf("Sandbox Action requires durable Job and Action identities")
	}
	return s.store.WithJobFence(ctx, jobID, func() error {
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		authorized, err := s.store.AuthorizeSandboxAction(ctx, actionID, task.TaskID(), task.TaskName())
		if err != nil {
			return err
		}
		authoritative := authorized.Action
		if authorized.Job.ID != jobID || authoritative.ID != actionID || authoritative.JobID != jobID || authorized.Sandbox.JobID != jobID || authoritative.Scope != authorized.Sandbox.ID ||
			authorized.TaskID != task.TaskID() || authorized.TaskName != task.TaskName() {
			return fmt.Errorf("Sandbox Action does not match its authoritative Job, Sandbox, and task")
		}
		if expectedKind != "" && authoritative.Kind != expectedKind {
			return fmt.Errorf("Sandbox Action %s is %s, not expected %s", actionID, authoritative.Kind, expectedKind)
		}
		if authoritative.Kind == ActionRouteRevoke || authoritative.Kind == ActionSandboxDelete {
			if err := s.rejectUnsettledHarnessMutations(ctx, jobID, string(authoritative.Kind)); err != nil {
				return err
			}
		}
		if authoritative.State == ActionSucceeded {
			return nil
		}
		err = effect(ctx, authorized)
		if err != nil {
			return err
		}
		point := ""
		switch authoritative.Kind {
		case ActionSandboxCreate:
			point = BarrierSandboxCreated
		case ActionRouteRevoke:
			point = BarrierRouteRevoked
		case ActionSandboxDelete:
			point = BarrierSandboxDeleted
		}
		if point != "" {
			if err := s.reachWorkflow(ctx, point, authorized.Job.ID, authoritative.ID); err != nil {
				return err
			}
		}
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		return s.store.RecordSandboxActionSuccess(ctx, authoritative.ID)
	})
}

// CompleteCleanup delegates the final locked terminal/mismatch/resource scan
// to the Store after revalidating the exact cleanup task under the Job fence.
func (s ExecutionService) CompleteCleanup(ctx context.Context, jobID string) error {
	return s.store.WithJobFence(ctx, jobID, func() error {
		if err := s.requireCleanupTask(ctx, jobID); err != nil {
			return err
		}
		task, ok := absurd.TaskFromContext(ctx)
		if !ok {
			return absurd.ErrNoTaskContext
		}
		return s.store.CompleteCleanup(ctx, jobID, task.TaskID())
	})
}

func (s ExecutionService) rejectUnsettledHarnessMutations(ctx context.Context, jobID, operation string) error {
	unsettled, err := s.store.UnsettledAgentMessages(ctx, jobID)
	if err != nil {
		return err
	}
	if len(unsettled) != 0 {
		return fmt.Errorf("%s rejected while %d Harness mutations remain unsettled", operation, len(unsettled))
	}
	return nil
}
