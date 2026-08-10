package spine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
)

type Store interface {
	Job(context.Context, string) (Job, error)
	Sandboxes(context.Context, string) ([]Sandbox, error)
	AgentRuns(context.Context, string) ([]AgentRun, error)
	InterruptAgentRun(context.Context, string, string) error
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
	RepositoryClone(context.Context, Job, Sandbox) error
	RouteCreate(context.Context, Job, Sandbox, Route) error
	AgentInitialTurn(context.Context, Job, Delivery) (HarnessBinding, error)
	AgentInitialTurns(context.Context, Job) (HarnessHistory, error)
	AgentTurns(context.Context, Job, string) (HarnessHistory, error)
	AgentSubmit(context.Context, Job, Delivery) (HarnessBinding, error)
	AgentSteer(context.Context, Job, Delivery) (string, error)
	AgentWait(context.Context, Job, string, string) (HarnessBinding, error)
	RouteRevoke(context.Context, Job, Sandbox, Route) error
	SandboxDelete(context.Context, Job, Sandbox) error
}

type CodingStore interface {
	RecordSetup(context.Context, string, Evidence, CommandObservation, []DeclaredCheck) error
	RecordRevisionObservation(context.Context, string, string, RevisionObservation, Evidence) (bool, error)
	RecordCheck(context.Context, Check, Evidence, CommandObservation) error
	AdmitCheckMessage(context.Context, Check) (Message, bool, error)
	SetWorkflowAttention(context.Context, string, string, string) error
	DeclaredChecks(context.Context, string) ([]DeclaredCheck, error)
	Checks(context.Context, string) ([]Check, error)
	Evidence(context.Context, string) ([]Evidence, error)
}

type RepositoryExternals interface {
	RepositorySetup(context.Context, Job, Action) (CommandObservation, []DeclaredCheck, error)
	RepositoryRevision(context.Context, Job) (RevisionObservation, error)
	RepositoryCheck(context.Context, Job, Check) (CommandObservation, error)
}

// ServiceStore and ServiceExternals are the one complete construction
// boundary for the coding service. Their embedded leaf interfaces stay small
// so focused logic and tests can depend on only the capability they use.
type ServiceStore interface {
	Store
	CodingStore
	ReviewStore
}

type ServiceExternals interface {
	Externals
	RepositoryExternals
	ReviewExternals
}

type FaultBarrier interface {
	Reach(context.Context, string, Delivery) error
	ReachWorkflow(context.Context, string, string, string) error
}

const commandEvidenceProducer = "dorf-command-observer"

const (
	BarrierBeforeSubmit          = "before-submit"
	BarrierAfterSubmitBeforeBind = "after-submit-before-bind"
	BarrierHarnessActive         = "harness-active"
	BarrierSetupComplete         = "setup-complete-before-record"
	BarrierCheckExited           = "check-exited-before-record"
	BarrierPushAccepted          = "push-accepted-before-record"
	BarrierPullRequestAccepted   = "pull-request-accepted-before-record"
	BarrierRouteRevoked          = "route-revoked-before-record"
	BarrierSandboxDeleted        = "sandbox-deleted-before-record"
)

type Service struct {
	store      ServiceStore
	externals  ServiceExternals
	barrier    FaultBarrier
	evidence   evidence.Store
	claimCheck func(context.Context) error
}

func NewService(store ServiceStore, externals ServiceExternals, records evidence.Store, barrier FaultBarrier, claimCheck func(context.Context) error) Service {
	return Service{
		store:      store,
		externals:  externals,
		barrier:    barrier,
		evidence:   records,
		claimCheck: claimCheck,
	}
}

func (s Service) EvidenceStore() evidence.Store { return s.evidence }

func (s Service) requireClaim(ctx context.Context) error {
	if s.claimCheck == nil {
		return errors.New("durable executor claim check is not configured")
	}
	return s.claimCheck(ctx)
}

func (s Service) recordAgentRun(ctx context.Context, record func() error) error {
	return claimBeforeAgentRunRecord(ctx, s.requireClaim, record)
}

func claimBeforeAgentRunRecord(ctx context.Context, claimCheck func(context.Context) error, record func() error) error {
	if err := claimCheck(ctx); err != nil {
		return err
	}
	return record()
}

func (s Service) agentRunAttention(ctx context.Context, runID, detail string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.AgentRunAttention(ctx, runID, detail) })
}

func (s Service) prepareAgentRun(ctx context.Context, runID, harness, baseline string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.PrepareAgentRun(ctx, runID, harness, baseline) })
}

func (s Service) bindAgentRun(ctx context.Context, runID, harness, threadID, turnID, outcome string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.BindAgentRun(ctx, runID, harness, threadID, turnID, outcome) })
}

func (s Service) bindSteer(ctx context.Context, runID, turnID, outcome string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.BindSteer(ctx, runID, turnID, outcome) })
}

func (s Service) failAgentRun(ctx context.Context, runID, reason string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.FailAgentRun(ctx, runID, reason) })
}

func (s Service) uncertainAgentRun(ctx context.Context, runID, reason string) error {
	return s.recordAgentRun(ctx, func() error { return s.store.UncertainAgentRun(ctx, runID, reason) })
}

func (s Service) ExecuteSetup(ctx context.Context, job Job, action Action) error {
	observation, declared, err := s.externals.RepositorySetup(ctx, job, action)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, action.ID, err)
		}
		return err
	}
	observation = canonicalCommandObservation(observation)
	artifact, err := commandArtifact(action.ID, job.StartingRevision, observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(action.ID, "repository-setup", action.ID, "", "", job.StartingRevision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.reachWorkflow(ctx, BarrierSetupComplete, job.ID, action.ID); err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	if err := s.store.RecordSetup(ctx, action.ID, evidenceRecord, observation, declared); err != nil {
		return err
	}
	return nil
}

func (s Service) ObserveRevision(ctx context.Context, job Job, run AgentRun) error {
	observation, err := s.externals.RepositoryRevision(ctx, job)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, run.ID, err)
		}
		return err
	}
	observation.StartedAt = observation.StartedAt.UTC().Truncate(time.Microsecond)
	observation.FinishedAt = observation.FinishedAt.UTC().Truncate(time.Microsecond)
	artifact, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(run.ID, "git-revision", "", run.ID, "", observation.Revision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	_, err = s.store.RecordRevisionObservation(ctx, job.ID, run.ID, observation, evidenceRecord)
	return err
}

func (s Service) ExecuteCheck(ctx context.Context, job Job, check Check) error {
	if check.State == "passed" {
		return nil
	}
	if check.State == "failed" {
		return s.handleFailedCheck(ctx, check)
	}
	observation, err := s.externals.RepositoryCheck(ctx, job, check)
	if err != nil {
		if attentionNeeded(err) {
			return s.setWorkflowAttention(ctx, job.ID, check.ID, err)
		}
		return err
	}
	observation = canonicalCommandObservation(observation)
	artifact, err := commandArtifact(check.ID, job.Revision, observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(check.ID, "check-output", "", "", check.ID, job.Revision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.reachWorkflow(ctx, BarrierCheckExited, job.ID, check.ID); err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	if err := s.store.RecordCheck(ctx, check, evidenceRecord, observation); err != nil {
		return err
	}
	check.State, check.ExitCode, check.EvidenceID = "passed", observation.ExitCode, evidenceRecord.ID
	if observation.ExitCode != 0 {
		check.State = "failed"
		return s.handleFailedCheck(ctx, check)
	}
	return nil
}

func (s Service) setWorkflowAttention(ctx context.Context, jobID, source string, cause error) error {
	if err := s.requireClaim(ctx); err != nil {
		return errors.Join(cause, err)
	}
	if err := s.store.SetWorkflowAttention(ctx, jobID, source, cause.Error()); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func attentionNeeded(err error) bool {
	var attention interface{ AttentionNeeded() bool }
	return errors.As(err, &attention) && attention.AttentionNeeded()
}

func (s Service) handleFailedCheck(ctx context.Context, check Check) error {
	if _, _, err := s.store.AdmitCheckMessage(ctx, check); err != nil {
		return err
	}
	return nil
}

func canonicalCommandObservation(observation CommandObservation) CommandObservation {
	observation.StartedAt = observation.StartedAt.UTC().Truncate(time.Microsecond)
	observation.FinishedAt = observation.FinishedAt.UTC().Truncate(time.Microsecond)
	return observation
}

func commandArtifact(identity, revision string, observation CommandObservation) ([]byte, error) {
	return json.Marshal(struct {
		Identity        string    `json:"identity"`
		Revision        string    `json:"revision"`
		Producer        string    `json:"producer"`
		Command         string    `json:"command"`
		ExitCode        int       `json:"exit_code"`
		StartedAt       time.Time `json:"started_at"`
		FinishedAt      time.Time `json:"finished_at"`
		Stdout          string    `json:"stdout"`
		Stderr          string    `json:"stderr"`
		StdoutTruncated bool      `json:"stdout_truncated"`
		StderrTruncated bool      `json:"stderr_truncated"`
		Redactions      []string  `json:"redactions"`
	}{identity, revision, commandEvidenceProducer, observation.Command, observation.ExitCode, observation.StartedAt, observation.FinishedAt, string(observation.Stdout), string(observation.Stderr), observation.StdoutCut, observation.StderrCut, observation.Redactions})
}

func (s Service) retainEvidence(ownerID, kind, actionID, agentRunID, checkID, revision string, startedAt, finishedAt time.Time, contents []byte) (Evidence, error) {
	blob, err := s.evidence.Put(contents)
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{ID: EvidenceID(ownerID, kind), Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: kind, ActionID: actionID, AgentRunID: agentRunID, CheckID: checkID, Revision: revision, StartedAt: startedAt.UTC().Truncate(time.Microsecond), FinishedAt: finishedAt.UTC().Truncate(time.Microsecond)}, nil
}

func (s Service) reachWorkflow(ctx context.Context, point, jobID, identity string) error {
	if s.barrier == nil {
		return nil
	}
	return s.barrier.ReachWorkflow(ctx, point, jobID, identity)
}

func (s Service) Deliver(ctx context.Context, job Job, delivery Delivery) error {
	if delivery.Message.Intent == MessageSteer && (delivery.AgentRun.TurnID == "" || delivery.AgentRun.TurnID == delivery.Message.TargetTurnID) {
		return s.deliverSteer(ctx, job, delivery)
	}
	return s.deliverAgentRun(ctx, job, delivery)
}

func (s Service) deliverAgentRun(ctx context.Context, job Job, delivery Delivery) error {
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
			return s.externals.AgentInitialTurn(ctx, job, delivery)
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
			return s.externals.AgentSubmit(ctx, job, delivery)
		},
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return s.externals.AgentTurns(ctx, job, run.ThreadID)
		},
		wait: func(ctx context.Context, run AgentRun, turnID string) (HarnessBinding, error) {
			return s.externals.AgentWait(ctx, job, run.ThreadID, turnID)
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

func (s Service) deliverSteer(ctx context.Context, job Job, delivery Delivery) error {
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
		return s.deliverAgentRun(ctx, job, delivery)
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
			return s.deliverAgentRun(ctx, job, delivery)
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

func (s Service) reach(ctx context.Context, point string, delivery Delivery) error {
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
func (s Service) PrepareCleanup(ctx context.Context, jobID string) (Job, []Sandbox, error) {
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
	if err := s.cleanupStep(ctx, job.ID, "reconciling any unsettled implementation harness mutation", func() error { return s.reconcileHarnessMutation(ctx, job) }); err != nil {
		return Job{}, nil, err
	}
	runs, err := s.store.AgentRuns(ctx, job.ID)
	if err != nil {
		_ = s.store.SetCleanupAttention(ctx, job.ID, "enumerating unsettled AgentRuns: "+err.Error())
		return Job{}, nil, err
	}
	for _, run := range runs {
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

func (s Service) cleanupStep(ctx context.Context, jobID, detail string, fn func() error) error {
	if err := s.store.SetCleanupAttention(ctx, jobID, detail); err != nil {
		return err
	}
	if err := fn(); err != nil {
		_ = s.store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
		return err
	}
	return nil
}

func emptyCleanupIdentity(value string) string {
	if value == "" {
		return "<not-recorded>"
	}
	return value
}

func (s Service) reconcileHarnessMutation(ctx context.Context, job Job) error {
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

// ExecuteSandboxAction reconciles one external mutation against its exact
// Sandbox. The caller uses the Action ID as the durable Absurd Step identity.
func (s Service) ExecuteSandboxAction(ctx context.Context, job Job, sandbox Sandbox, action Action) error {
	if sandbox.JobID != job.ID || action.JobID != job.ID || action.Scope != sandbox.ID {
		return fmt.Errorf("Sandbox Action does not belong to the exact Job and Sandbox")
	}
	var err error
	switch action.Kind {
	case ActionSandboxCreate:
		err = s.externals.SandboxCreate(ctx, job, sandbox)
	case ActionRepositoryClone:
		err = s.externals.RepositoryClone(ctx, job, sandbox)
	case ActionRouteCreate, ActionRouteRevoke:
		route := RouteForSandbox(sandbox)
		if action.Kind == ActionRouteCreate {
			err = s.externals.RouteCreate(ctx, job, sandbox, route)
		}
		if action.Kind == ActionRouteRevoke {
			err = s.externals.RouteRevoke(ctx, job, sandbox, route)
		}
	case ActionSandboxDelete:
		err = s.externals.SandboxDelete(ctx, job, sandbox)
	default:
		return fmt.Errorf("unsupported Sandbox Action kind %q", action.Kind)
	}
	if err != nil {
		return err
	}
	point := ""
	if action.Kind == ActionRouteRevoke {
		point = BarrierRouteRevoked
	}
	if action.Kind == ActionSandboxDelete {
		point = BarrierSandboxDeleted
	}
	if point != "" {
		if err := s.reachWorkflow(ctx, point, job.ID, action.ID); err != nil {
			return err
		}
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	if err := s.store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
		return err
	}
	return nil
}
