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
	Sandboxes(context.Context, string) ([]Sandbox, error)
	Deliveries(context.Context, string) ([]Delivery, error)
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
	HarnessMutationDelivery(context.Context, string) (*Delivery, error)
	SetCleanupAttention(context.Context, string, string) error
}

type Externals interface {
	Harness() string
	SandboxCreate(context.Context, Job, Sandbox) error
	RouteCreate(context.Context, Job, Sandbox, Route) error
	AgentInitialTurn(context.Context, Job, Delivery, string) (HarnessBinding, error)
	AgentInitialTurns(context.Context, Job) (HarnessHistory, error)
	AgentTurns(context.Context, Job, string) (HarnessHistory, error)
	AgentSubmit(context.Context, Job, Delivery, string) (HarnessBinding, error)
	AgentSteer(context.Context, Job, Delivery) (string, error)
	AgentWait(context.Context, Job, string, string) (HarnessBinding, error)
	RouteRevoke(context.Context, Job, Sandbox, Route) error
	SandboxDelete(context.Context, Job, Sandbox) error
}

type FaultBarrier interface {
	Reach(context.Context, string, Delivery) error
	ReachWorkflow(context.Context, string, string, string) error
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

func (s ExecutionService) Deliver(ctx context.Context, job Job, delivery Delivery, input string) error {
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
			history, err := s.externals.AgentInitialTurns(ctx, job)
			if err != nil || history.ThreadID == "" || len(history.Turns) == 0 {
				return HarnessBinding{}, err
			}
			return HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: history.Turns[len(history.Turns)-1], ControllerID: history.ControllerID}, nil
		},
		submitBound: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			delivery.AgentRun = run
			return s.externals.AgentSubmit(ctx, job, delivery, input)
		},
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return s.externals.AgentTurns(ctx, job, run.ThreadID)
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

// ObserveAgentRunTurn reconciles one bound ordinary Harness Turn for the
// workflow-owned role and returns its current or terminal native result.
func (s ExecutionService) ObserveAgentRunTurn(ctx context.Context, job Job, run AgentRun, role string) (HarnessTurn, error) {
	if run.JobID != job.ID || run.Role != role || run.ThreadID == "" || run.TurnID == "" {
		return HarnessTurn{}, fmt.Errorf("AgentRun %s is not an exact bound %s Turn for Job %s", run.ID, role, job.ID)
	}
	contract := agentRunContract{
		store:   s.store,
		run:     run,
		harness: s.externals.Harness(),
		label:   "harness",
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return s.externals.AgentTurns(ctx, job, run.ThreadID)
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
	history, err := s.externals.AgentTurns(ctx, job, run.ThreadID)
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
		observedHistory, inspectErr := s.externals.AgentTurns(ctx, job, run.ThreadID)
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
	job, err := s.store.Job(ctx, jobID)
	if err != nil {
		return Job{}, nil, err
	}
	if job.AdmissionOpen {
		return Job{}, nil, fmt.Errorf("cleanup recovery requires closed admission and a stopped ordinary run")
	}
	if job.CleanupState == CleanupComplete {
		return job, nil, nil
	}
	if job.CleanupState != CleanupScheduled {
		return Job{}, nil, fmt.Errorf("cleanup recovery requires a durably scheduled cleanup task")
	}
	if err := s.cleanupStep(ctx, job.ID, "reconciling any unsettled harness mutation", func() error { return s.reconcileHarnessMutation(ctx, job) }); err != nil {
		return Job{}, nil, err
	}
	deliveries, err := s.store.Deliveries(ctx, job.ID)
	if err != nil {
		_ = s.store.SetCleanupAttention(ctx, job.ID, "enumerating unsettled AgentRuns: "+err.Error())
		return Job{}, nil, err
	}
	for _, delivery := range deliveries {
		run := delivery.AgentRun
		settled := run.State == AgentRunCompleted || run.State == AgentRunFailed || run.State == AgentRunInterrupted
		if !settled {
			if err := s.store.InterruptAgentRun(ctx, run.ID, "admission closed; Job resources are being reclaimed"); err != nil {
				_ = s.store.SetCleanupAttention(ctx, job.ID, "interrupting unsettled AgentRun "+run.ID+": "+err.Error())
				return Job{}, nil, err
			}
		}
	}
	sandboxes, err := s.store.Sandboxes(ctx, job.ID)
	if err != nil {
		return Job{}, nil, err
	}
	return job, sandboxes, nil
}

func (s ExecutionService) cleanupStep(ctx context.Context, jobID, detail string, fn func() error) error {
	if err := s.store.SetCleanupAttention(ctx, jobID, detail); err != nil {
		return err
	}
	if err := fn(); err != nil {
		_ = s.store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
		return err
	}
	return nil
}

func (s ExecutionService) reconcileHarnessMutation(ctx context.Context, job Job) error {
	delivery, err := s.store.HarnessMutationDelivery(ctx, job.ID)
	if err != nil || delivery == nil {
		return err
	}
	run := delivery.AgentRun
	if run.State == AgentRunUncertain {
		return cleanupBlocked(*delivery, run.Attention)
	}
	threadID := run.ThreadID
	var history HarnessHistory
	if threadID == "" {
		history, err = s.externals.AgentInitialTurns(ctx, job)
		if err == nil && history.ThreadID == "" && len(history.Turns) > 0 {
			err = fmt.Errorf("cleanup initial history returned turns without a harness thread")
		}
		if err == nil && history.ThreadID != "" && len(history.Turns) > 0 {
			threadID = history.ThreadID
			run.Harness, run.ThreadID = history.Harness, history.ThreadID
			delivery.AgentRun = run
		}
	} else {
		history, err = s.externals.AgentTurns(ctx, job, threadID)
	}
	if err != nil {
		var attention interface{ AttentionNeeded() bool }
		if errors.As(err, &attention) && attention.AttentionNeeded() {
			if persistErr := s.uncertainAgentRun(ctx, run.ID, err.Error()); persistErr != nil {
				return persistErr
			}
			return cleanupBlocked(*delivery, err.Error())
		}
		reason := "cleanup could not inspect the bound harness thread: " + err.Error()
		_ = s.agentRunAttention(ctx, run.ID, reason)
		return cleanupBlocked(*delivery, reason)
	}
	turns := history.Turns
	if delivery.Message.Intent == MessageSteer && (run.TurnID == "" || run.TurnID == delivery.Message.TargetTurnID) {
		reconciliation := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, turns)
		switch reconciliation.Classification {
		case "completed":
			return s.bindSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciliation.Turn.Status)
		case "no-submit":
			return s.failAgentRun(ctx, run.ID, "cleanup closed steer delivery after harness history proved it was not accepted")
		case "target-terminal":
			if !run.BaselineRecorded {
				return s.failAgentRun(ctx, run.ID, "cleanup closed steer delivery after harness history proved it was not accepted")
			}
		default:
			if err := s.uncertainAgentRun(ctx, run.ID, reconciliation.Reason); err != nil {
				return err
			}
			return cleanupBlocked(*delivery, reconciliation.Reason)
		}
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.TurnID, turns)
	switch reconciliation.Classification {
	case "no-submit":
		reason := "cleanup closed delivery after harness history proved no turn was submitted"
		if err := s.failAgentRun(ctx, run.ID, reason); err != nil {
			return err
		}
		return nil
	case "uncertain":
		if reconciliation.Turn.ID != "" {
			if err := s.bindAgentRun(ctx, run.ID, history.Harness, history.ThreadID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
				return err
			}
		} else if err := s.uncertainAgentRun(ctx, run.ID, reconciliation.Reason); err != nil {
			return err
		}
		return cleanupBlocked(*delivery, reconciliation.Reason)
	}
	if err := s.bindAgentRun(ctx, run.ID, history.Harness, history.ThreadID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
		return err
	}
	if reconciliation.Classification != "active" {
		return nil
	}
	outcome, err := s.externals.AgentWait(ctx, job, threadID, reconciliation.Turn.ID)
	if err != nil {
		reason := "cleanup is waiting for the exact harness turn outcome: " + err.Error()
		_ = s.agentRunAttention(ctx, run.ID, reason)
		return cleanupBlocked(*delivery, reason)
	}
	if err := s.bindAgentRun(ctx, run.ID, outcome.Harness, outcome.ThreadID, outcome.Turn.ID, outcome.Turn.Status); err != nil {
		return err
	}
	if !terminalHarness(outcome.Turn.Status) {
		reason := fmt.Sprintf("cleanup inspection returned nonterminal harness status %q", outcome.Turn.Status)
		_ = s.agentRunAttention(ctx, run.ID, reason)
		return cleanupBlocked(*delivery, reason)
	}
	return nil
}

func cleanupBlocked(delivery Delivery, reason string) error {
	if reason == "" {
		reason = string(delivery.AgentRun.State)
	}
	return fmt.Errorf("cleanup retained Sandbox and route: message sequence %d is not safely settled (%s)", delivery.Message.Sequence, reason)
}

// ExecuteSandboxAction reconciles one authoritative persisted mutation. The
// caller supplies only the Job and Action identities used by durable custody.
func (s ExecutionService) ExecuteSandboxAction(ctx context.Context, jobID, actionID string) error {
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
		if authoritative.State == ActionSucceeded {
			return nil
		}
		switch authoritative.Kind {
		case ActionSandboxCreate:
			err = s.externals.SandboxCreate(ctx, authorized.Job, authorized.Sandbox)
		case ActionRouteCreate, ActionRouteRevoke:
			route := RouteForSandbox(authorized.Sandbox)
			if authoritative.Kind == ActionRouteCreate {
				err = s.externals.RouteCreate(ctx, authorized.Job, authorized.Sandbox, route)
			} else {
				err = s.externals.RouteRevoke(ctx, authorized.Job, authorized.Sandbox, route)
			}
		case ActionSandboxDelete:
			err = s.externals.SandboxDelete(ctx, authorized.Job, authorized.Sandbox)
		default:
			return fmt.Errorf("unsupported Sandbox Action kind %q", authoritative.Kind)
		}
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
