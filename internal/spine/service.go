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
	WithJobFence(context.Context, string, func() error) error
	Sandbox(context.Context, string) (Sandbox, error)
	Sandboxes(context.Context, string) ([]Sandbox, error)
	AgentRuns(context.Context, string) ([]AgentRun, error)
	GetOrCreateSandboxAction(context.Context, string, ActionKind) (Action, error)
	InterruptAgentRun(context.Context, string, string) error
	BeginSetup(context.Context, string) (Action, error)
	RecordActionSuccess(context.Context, string, Receipt) error
	UncertainAction(context.Context, string) error
	NextDelivery(context.Context, string) (*Delivery, error)
	PrepareAgentRun(context.Context, string, string, string) error
	BindAgentRun(context.Context, string, string, string, string, string) error
	BindSteer(context.Context, string, string, string) error
	FailAgentRun(context.Context, string, string) error
	UncertainAgentRun(context.Context, string, string) error
	AgentRunAttention(context.Context, string, string) error
	HarnessMutationDelivery(context.Context, string) (*Delivery, error)
	SetCleanupAttention(context.Context, string, string) error
	CompleteCleanup(context.Context, string) error
}

type Externals interface {
	Harness() string
	SandboxCreate(context.Context, Job, Sandbox, Action) (Receipt, error)
	RepositoryClone(context.Context, Job, Sandbox, Action) (Receipt, error)
	RouteCreate(context.Context, Job, Sandbox, Route, Action) (Receipt, error)
	AgentInitialTurn(context.Context, Job, Delivery) (HarnessBinding, error)
	AgentInitialTurns(context.Context, Job) (HarnessHistory, error)
	AgentTurns(context.Context, Job, string) (HarnessHistory, error)
	AgentSubmit(context.Context, Job, Delivery) (HarnessBinding, error)
	AgentSteer(context.Context, Job, Delivery) (string, error)
	AgentWait(context.Context, Job, string, string) (HarnessBinding, error)
	RouteRevoke(context.Context, Job, Sandbox, Route, Action) (Receipt, error)
	SandboxDelete(context.Context, Job, Sandbox, Action) (Receipt, error)
}

type CodingStore interface {
	RevisionCandidate(context.Context, string, string) (AgentRun, bool, error)
	CompleteUnchangedRun(context.Context, string, string, string, string) (bool, error)
	RecordSetup(context.Context, string, Evidence, CommandObservation, []DeclaredCheck) error
	RecordRevision(context.Context, string, string, RevisionObservation, Evidence) (bool, error)
	BeginCheck(context.Context, string, string, string, string) (Check, error)
	RecordCheck(context.Context, Check, Evidence, CommandObservation) error
	AdmitCheckMessage(context.Context, Check) (Message, bool, error)
	MarkReady(context.Context, string, string, []string) error
	BlockWorkflow(context.Context, string, string) error
	DeclaredChecks(context.Context, string) ([]DeclaredCheck, error)
	Checks(context.Context, string) ([]Check, error)
	Evidence(context.Context, string) ([]Evidence, error)
}

type RepositoryExternals interface {
	RepositorySetup(context.Context, Job, Action) (CommandObservation, []DeclaredCheck, error)
	RepositoryRevision(context.Context, Job) (RevisionObservation, []byte, error)
	RepositoryCheck(context.Context, Job, Check) (CommandObservation, error)
}

type FaultBarrier interface {
	Reach(context.Context, string, Delivery) error
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

type RunDisposition string

const (
	RunIdle    RunDisposition = "idle"
	RunBlocked RunDisposition = "blocked"
	RunClosed  RunDisposition = "closed"
)

type Service struct {
	Store      Store
	Externals  Externals
	Barrier    FaultBarrier
	Repository RepositoryExternals
	Evidence   evidence.Store
	ClaimCheck func(context.Context) error
}

func (s Service) requireClaim(ctx context.Context) error {
	if s.ClaimCheck == nil {
		return nil
	}
	return s.ClaimCheck(ctx)
}

func (s Service) ExecuteSetup(ctx context.Context, job Job, action Action) error {
	store := s.Store.(CodingStore)
	observation, declared, err := s.Repository.RepositorySetup(ctx, job, action)
	if err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		if attentionNeeded(err) {
			_ = store.BlockWorkflow(ctx, job.ID, err.Error())
			return nil
		}
		return err
	}
	observation = canonicalCommandObservation(observation)
	artifact, err := commandArtifact(action.ID, job.StartingRevision, observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(action.ID, "repository-setup", action.ID, "", job.StartingRevision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.reachWorkflow(ctx, BarrierSetupComplete, job.ID, action.ID); err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		return err
	}
	if err := store.RecordSetup(ctx, action.ID, evidenceRecord, observation, declared); err != nil {
		return err
	}
	if observation.ExitCode != 0 {
		return nil
	}
	return nil
}

func (s Service) ObserveRevision(ctx context.Context, job Job, run AgentRun) error {
	store := s.Store.(CodingStore)
	observation, artifact, err := s.Repository.RepositoryRevision(ctx, job)
	if err != nil {
		if attentionNeeded(err) {
			_ = store.BlockWorkflow(ctx, job.ID, err.Error())
			return nil
		}
		return err
	}
	if observation.Revision == observation.ComparisonBase {
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		if job.WorkflowPhase == "review-feedback" {
			reviewStore := s.Store.(ReviewStore)
			if _, err := reviewStore.CompleteReviewFeedback(ctx, job.ID, run.ID, job.Revision); err != nil {
				return err
			}
			return nil
		}
		reason := fmt.Sprintf("AgentRun %s completed without a new committed Revision", run.ID)
		blocked, err := store.CompleteUnchangedRun(ctx, job.ID, run.ID, observation.ComparisonBase, reason)
		if err != nil {
			return err
		}
		if !blocked {
			return nil
		}
		return nil
	}
	evidenceRecord, err := s.retainEvidence(run.ID, "git-revision", "", "", observation.Revision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	recorded, err := store.RecordRevision(ctx, job.ID, run.ID, observation, evidenceRecord)
	if err != nil {
		return err
	}
	if !recorded {
		return nil
	}
	return nil
}

func (s Service) ExecuteCheck(ctx context.Context, job Job, check Check) error {
	store := s.Store.(CodingStore)
	if check.State == "passed" {
		return nil
	}
	if check.State == "failed" {
		return s.HandleFailedCheck(ctx, job, check)
	}
	observation, err := s.Repository.RepositoryCheck(ctx, job, check)
	if err != nil {
		if attentionNeeded(err) {
			_ = store.BlockWorkflow(ctx, job.ID, err.Error())
			return nil
		}
		return err
	}
	observation = canonicalCommandObservation(observation)
	artifact, err := commandArtifact(check.ID, job.Revision, observation)
	if err != nil {
		return err
	}
	evidenceRecord, err := s.retainEvidence(check.ID, "check-output", "", check.ID, job.Revision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return err
	}
	if err := s.reachWorkflow(ctx, BarrierCheckExited, job.ID, check.ID); err != nil {
		return err
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	if err := store.RecordCheck(ctx, check, evidenceRecord, observation); err != nil {
		return err
	}
	check.State, check.ExitCode, check.EvidenceID, check.EvidenceDigest = "passed", observation.ExitCode, evidenceRecord.ID, evidenceRecord.Digest
	if observation.ExitCode != 0 {
		check.State = "failed"
		return s.HandleFailedCheck(ctx, job, check)
	}
	return nil
}

func (s Service) VerifyChecks(ctx context.Context, job Job, declared []DeclaredCheck) error {
	store := s.Store.(CodingStore)
	checks, err := store.Checks(ctx, job.ID)
	if err != nil {
		return err
	}
	records, err := store.Evidence(ctx, job.ID)
	if err != nil {
		return err
	}
	verified, err := VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, s.Evidence)
	if err != nil {
		reason := fmt.Sprintf("Revision %s Evidence verification failed: %v", job.Revision, err)
		if blockErr := store.BlockWorkflow(ctx, job.ID, reason); blockErr != nil {
			return blockErr
		}
		return nil
	}
	verifiedIDs := make([]string, 0, len(verified))
	for _, result := range verified {
		verifiedIDs = append(verifiedIDs, result.EvidenceID)
	}
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	if reviewStore, ok := s.Store.(ReviewStore); ok {
		if err := reviewStore.MarkChecksVerified(ctx, job.ID, job.Revision, verifiedIDs); err != nil {
			return err
		}
	} else if err := store.MarkReady(ctx, job.ID, job.Revision, verifiedIDs); err != nil {
		return err
	}
	return nil
}

func attentionNeeded(err error) bool {
	var attention interface{ AttentionNeeded() bool }
	return errors.As(err, &attention) && attention.AttentionNeeded()
}

func (s Service) HandleFailedCheck(ctx context.Context, job Job, check Check) error {
	store := s.Store.(CodingStore)
	if _, _, err := store.AdmitCheckMessage(ctx, check); err != nil {
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

func (s Service) retainEvidence(ownerID, kind, actionID, checkID, revision string, startedAt, finishedAt time.Time, contents []byte) (Evidence, error) {
	blob, err := s.Evidence.Put(contents)
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{ID: EvidenceID(ownerID, kind), Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Kind: kind, ActionID: actionID, CheckID: checkID, Revision: revision, StartedAt: startedAt.UTC().Truncate(time.Microsecond), FinishedAt: finishedAt.UTC().Truncate(time.Microsecond)}, nil
}

func (s Service) reachWorkflow(ctx context.Context, point, jobID, identity string) error {
	barrier, ok := s.Barrier.(interface {
		ReachWorkflow(context.Context, string, string, string) error
	})
	if !ok {
		return nil
	}
	return barrier.ReachWorkflow(ctx, point, jobID, identity)
}

func (s Service) Deliver(ctx context.Context, job Job, delivery Delivery) error {
	if delivery.Message.Intent == MessageSteer && (delivery.AgentRun.TurnID == "" || delivery.AgentRun.TurnID == delivery.Message.TargetTurnID) {
		_, err := s.deliverSteer(ctx, job, delivery)
		return err
	}
	_, err := s.deliverAgentRun(ctx, job, delivery)
	return err
}

func (s Service) deliverAgentRun(ctx context.Context, job Job, delivery Delivery) (bool, error) {
	run := delivery.AgentRun
	contract := agentRunContract{
		service:             s,
		delivery:            delivery,
		run:                 run,
		harness:             s.Externals.Harness(),
		label:               "harness",
		bindUnsupportedTurn: true,
		submitNew: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			delivery.AgentRun = run
			return s.Externals.AgentInitialTurn(ctx, job, delivery)
		},
		recover: func(ctx context.Context, _ AgentRun) (HarnessBinding, error) {
			history, err := s.Externals.AgentInitialTurns(ctx, job)
			if err != nil || history.ThreadID == "" || len(history.Turns) == 0 {
				return HarnessBinding{}, err
			}
			return HarnessBinding{Harness: history.Harness, ThreadID: history.ThreadID, Turn: history.Turns[len(history.Turns)-1], ControllerID: history.ControllerID}, nil
		},
		submitBound: func(ctx context.Context, run AgentRun) (HarnessBinding, error) {
			delivery.AgentRun = run
			return s.Externals.AgentSubmit(ctx, job, delivery)
		},
		history: func(ctx context.Context, run AgentRun) (HarnessHistory, error) {
			return s.Externals.AgentTurns(ctx, job, run.ThreadID)
		},
		wait: func(ctx context.Context, run AgentRun, turnID string) (HarnessBinding, error) {
			return s.Externals.AgentWait(ctx, job, run.ThreadID, turnID)
		},
		beforeBind: s.requireClaim,
		onReadError: func(ctx context.Context, runID string, err error) {
			_ = s.Store.AgentRunAttention(ctx, runID, "harness thread or submitted turn is currently unavailable: "+err.Error())
		},
		onSubmitError: func(ctx context.Context, run AgentRun, _ HarnessBinding, err error) (HarnessTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if failErr := s.Store.FailAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return HarnessTurn{}, failErr
				}
			}
			if attentionNeeded(err) {
				if uncertainErr := s.Store.UncertainAgentRun(ctx, run.ID, err.Error()); uncertainErr != nil {
					return HarnessTurn{}, uncertainErr
				}
			}
			return HarnessTurn{}, err
		},
	}
	outcome, err := contract.execute(ctx)
	if err != nil {
		return false, err
	}
	return terminalHarness(outcome.Status), nil
}

func (s Service) deliverSteer(ctx context.Context, job Job, delivery Delivery) (bool, error) {
	run := delivery.AgentRun
	history, err := s.Externals.AgentTurns(ctx, job, run.ThreadID)
	if err != nil {
		_ = s.Store.AgentRunAttention(ctx, run.ID, "harness thread history is currently unavailable: "+err.Error())
		return false, err
	}
	turns := history.Turns
	reconciliation := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, turns)
	if reconciliation.Classification == "completed" {
		if err := s.requireClaim(ctx); err != nil {
			return false, err
		}
		return true, s.Store.BindSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciliation.Turn.Status)
	}
	if reconciliation.Classification == "target-terminal" {
		if !run.BaselineRecorded && turns[len(turns)-1].ID != delivery.Message.TargetTurnID {
			return false, s.Store.UncertainAgentRun(ctx, run.ID, "harness turns appeared after the terminal steer target before a fallback baseline was recorded")
		}
		return s.deliverAgentRun(ctx, job, delivery)
	}
	if reconciliation.Classification == "uncertain" {
		return false, s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason)
	}
	if !run.BaselineRecorded {
		if err := s.Store.PrepareAgentRun(ctx, run.ID, run.Harness, delivery.Message.TargetTurnID); err != nil {
			return false, err
		}
		delivery.AgentRun.BaselineRecorded = true
		delivery.AgentRun.BaselineTurnID = delivery.Message.TargetTurnID
	}
	if err := s.reach(ctx, BarrierBeforeSubmit, delivery); err != nil {
		return false, err
	}
	acceptedTurnID, err := s.Externals.AgentSteer(ctx, job, delivery)
	if err != nil {
		observedHistory, inspectErr := s.Externals.AgentTurns(ctx, job, run.ThreadID)
		if inspectErr != nil {
			reason := "harness steer acknowledgement is genuinely uncertain: " + err.Error() + "; history inspection failed: " + inspectErr.Error()
			return false, s.Store.UncertainAgentRun(ctx, run.ID, reason)
		}
		observed := observedHistory.Turns
		reconciled := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, observed)
		if reconciled.Classification == "completed" {
			if claimErr := s.requireClaim(ctx); claimErr != nil {
				return false, claimErr
			}
			return true, s.Store.BindSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciled.Turn.Status)
		}
		if reconciled.Classification == "target-terminal" {
			if !delivery.AgentRun.BaselineRecorded && observed[len(observed)-1].ID != delivery.Message.TargetTurnID {
				return false, s.Store.UncertainAgentRun(ctx, run.ID, "harness turns appeared after the terminal steer target before a fallback baseline was recorded")
			}
			return s.deliverAgentRun(ctx, job, delivery)
		}
		if reconciled.Classification == "uncertain" {
			return false, s.Store.UncertainAgentRun(ctx, run.ID, reconciled.Reason)
		}
		return false, err
	}
	if acceptedTurnID != delivery.Message.TargetTurnID {
		return false, s.Store.UncertainAgentRun(ctx, run.ID, "harness steer acknowledgement named a different active turn")
	}
	if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
		return false, err
	}
	if err := s.requireClaim(ctx); err != nil {
		return false, err
	}
	return true, s.Store.BindSteer(ctx, run.ID, acceptedTurnID, reconciliation.Turn.Status)
}

func (s Service) reach(ctx context.Context, point string, delivery Delivery) error {
	if s.Barrier == nil {
		return nil
	}
	return s.Barrier.Reach(ctx, point, delivery)
}

func terminalHarness(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func activeHarness(status string) bool {
	// "inProgress" is the app-server thread/read spelling. "running" is
	// Dorf's local status immediately after turn/start acceptance.
	return status == "running" || status == "inProgress"
}

func (s Service) Cleanup(ctx context.Context, jobID string) error {
	job, err := s.Store.Job(ctx, jobID)
	if err != nil {
		return err
	}
	if job.AdmissionOpen {
		return fmt.Errorf("cleanup recovery requires closed admission and a stopped ordinary run")
	}
	if job.CleanupState == CleanupComplete {
		return nil
	}
	if err := s.cleanupStep(ctx, job.ID, "reconciling any unsettled implementation harness mutation", func() error { return s.reconcileHarnessMutation(ctx, job) }); err != nil {
		return err
	}
	runs, err := s.Store.AgentRuns(ctx, job.ID)
	if err != nil {
		_ = s.Store.SetCleanupAttention(ctx, job.ID, "enumerating unsettled AgentRuns: "+err.Error())
		return err
	}
	for _, run := range runs {
		settled := run.State == AgentRunCompleted || run.State == AgentRunFailed || run.State == AgentRunInterrupted
		if !settled {
			if err := s.Store.InterruptAgentRun(ctx, run.ID, "admission closed; Job resources are being reclaimed"); err != nil {
				_ = s.Store.SetCleanupAttention(ctx, job.ID, "interrupting unsettled AgentRun "+run.ID+": "+err.Error())
				return err
			}
		}
	}
	sandboxes, err := s.Store.Sandboxes(ctx, job.ID)
	if err != nil {
		return err
	}
	for _, sandbox := range sandboxes {
		for _, kind := range []ActionKind{ActionRouteRevoke, ActionSandboxDelete} {
			detail := fmt.Sprintf("reconciling %s for Sandbox %s", kind, sandbox.ID)
			if err := s.cleanupStep(ctx, job.ID, detail, func() error { _, err := s.reconcileSandboxAction(ctx, job, sandbox, kind); return err }); err != nil {
				return fmt.Errorf("reconcile %s for Sandbox %s: %w", kind, sandbox.ID, err)
			}
		}
	}
	return s.cleanupStep(ctx, job.ID, "verifying no owned resource or non-cleanup Job claim remains unsettled", func() error { return s.Store.CompleteCleanup(ctx, job.ID) })
}

func (s Service) cleanupStep(ctx context.Context, jobID, detail string, fn func() error) error {
	if err := s.Store.SetCleanupAttention(ctx, jobID, detail); err != nil {
		return err
	}
	if err := fn(); err != nil {
		_ = s.Store.SetCleanupAttention(ctx, jobID, detail+": "+err.Error())
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
	delivery, err := s.Store.HarnessMutationDelivery(ctx, job.ID)
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
		history, err = s.Externals.AgentInitialTurns(ctx, job)
		if err == nil && history.ThreadID == "" && len(history.Turns) > 0 {
			err = fmt.Errorf("cleanup initial history returned turns without a harness thread")
		}
		if err == nil && history.ThreadID != "" && len(history.Turns) > 0 {
			threadID = history.ThreadID
			run.Harness, run.ThreadID = history.Harness, history.ThreadID
			delivery.AgentRun = run
		}
	} else {
		history, err = s.Externals.AgentTurns(ctx, job, threadID)
	}
	if err != nil {
		var attention interface{ AttentionNeeded() bool }
		if errors.As(err, &attention) && attention.AttentionNeeded() {
			if persistErr := s.Store.UncertainAgentRun(ctx, run.ID, err.Error()); persistErr != nil {
				return persistErr
			}
			return cleanupBlocked(*delivery, err.Error())
		}
		reason := "cleanup could not inspect the bound harness thread: " + err.Error()
		_ = s.Store.AgentRunAttention(ctx, run.ID, reason)
		return cleanupBlocked(*delivery, reason)
	}
	turns := history.Turns
	if delivery.Message.Intent == MessageSteer && (run.TurnID == "" || run.TurnID == delivery.Message.TargetTurnID) {
		reconciliation := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, turns)
		switch reconciliation.Classification {
		case "completed":
			return s.Store.BindSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciliation.Turn.Status)
		case "no-submit":
			return s.Store.FailAgentRun(ctx, run.ID, "cleanup closed steer delivery after harness history proved it was not accepted")
		case "target-terminal":
			if !run.BaselineRecorded {
				return s.Store.FailAgentRun(ctx, run.ID, "cleanup closed steer delivery after harness history proved it was not accepted")
			}
		default:
			if err := s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason); err != nil {
				return err
			}
			return cleanupBlocked(*delivery, reconciliation.Reason)
		}
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.TurnID, turns)
	switch reconciliation.Classification {
	case "no-submit":
		reason := "cleanup closed delivery after harness history proved no turn was submitted"
		if err := s.Store.FailAgentRun(ctx, run.ID, reason); err != nil {
			return err
		}
		return nil
	case "uncertain":
		if reconciliation.Turn.ID != "" {
			if err := s.Store.BindAgentRun(ctx, run.ID, history.Harness, history.ThreadID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
				return err
			}
		} else if err := s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason); err != nil {
			return err
		}
		return cleanupBlocked(*delivery, reconciliation.Reason)
	}
	if err := s.Store.BindAgentRun(ctx, run.ID, history.Harness, history.ThreadID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
		return err
	}
	if reconciliation.Classification != "active" {
		return nil
	}
	outcome, err := s.Externals.AgentWait(ctx, job, threadID, reconciliation.Turn.ID)
	if err != nil {
		reason := "cleanup is waiting for the exact harness turn outcome: " + err.Error()
		_ = s.Store.AgentRunAttention(ctx, run.ID, reason)
		return cleanupBlocked(*delivery, reason)
	}
	if err := s.Store.BindAgentRun(ctx, run.ID, outcome.Harness, outcome.ThreadID, outcome.Turn.ID, outcome.Turn.Status); err != nil {
		return err
	}
	if !terminalHarness(outcome.Turn.Status) {
		reason := fmt.Sprintf("cleanup inspection returned nonterminal harness status %q", outcome.Turn.Status)
		_ = s.Store.AgentRunAttention(ctx, run.ID, reason)
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

func (s Service) reconcileSandboxAction(ctx context.Context, job Job, sandbox Sandbox, kind ActionKind) (Receipt, error) {
	action, err := s.Store.GetOrCreateSandboxAction(ctx, sandbox.ID, kind)
	if err != nil {
		return Receipt{}, err
	}
	if action.State == ActionSucceeded {
		return Receipt{ExternalID: action.ExternalID, Outcome: action.Outcome}, nil
	}
	return s.ExecuteSandboxAction(ctx, job, sandbox, action)
}

// ExecuteSandboxAction reconciles one external mutation against its exact
// Sandbox. The caller uses the Action ID as the durable Absurd Step identity.
func (s Service) ExecuteSandboxAction(ctx context.Context, job Job, sandbox Sandbox, action Action) (Receipt, error) {
	if sandbox.JobID != job.ID || action.JobID != job.ID || action.Scope != sandbox.ID {
		return Receipt{}, fmt.Errorf("Sandbox Action does not belong to the exact Job and Sandbox")
	}
	var receipt Receipt
	var err error
	switch action.Kind {
	case ActionSandboxCreate:
		receipt, err = s.Externals.SandboxCreate(ctx, job, sandbox, action)
	case ActionRepositoryClone:
		receipt, err = s.Externals.RepositoryClone(ctx, job, sandbox, action)
	case ActionRouteCreate, ActionRouteRevoke:
		route := RouteForSandbox(sandbox)
		if action.Kind == ActionRouteCreate {
			receipt, err = s.Externals.RouteCreate(ctx, job, sandbox, route, action)
		}
		if action.Kind == ActionRouteRevoke {
			receipt, err = s.Externals.RouteRevoke(ctx, job, sandbox, route, action)
		}
	case ActionSandboxDelete:
		receipt, err = s.Externals.SandboxDelete(ctx, job, sandbox, action)
	default:
		return Receipt{}, fmt.Errorf("unsupported Sandbox Action kind %q", action.Kind)
	}
	if err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		return Receipt{}, err
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
			return Receipt{}, err
		}
	}
	if err := s.requireClaim(ctx); err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		return Receipt{}, err
	}
	if err := s.Store.RecordActionSuccess(ctx, action.ID, receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
