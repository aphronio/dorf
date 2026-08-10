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
	BeginAction(context.Context, string, ActionKind) (Action, error)
	BeginSetup(context.Context, string) (Action, error)
	CompleteAction(context.Context, string, Receipt) error
	UncertainAction(context.Context, string) error
	NextDelivery(context.Context, string, string) (*Delivery, error)
	PrepareAgentRun(context.Context, string, string) error
	BeginTurnSubmission(context.Context, string) error
	BindNativeTurn(context.Context, string, string, string) error
	FailAgentRun(context.Context, string, string) error
	UncertainAgentRun(context.Context, string, string) error
	AgentRunAttention(context.Context, string, string) error
	NativeMutationDelivery(context.Context, string) (*Delivery, error)
	SetCleanupAttention(context.Context, string, string) error
	CompleteCleanup(context.Context, string) error
}

type Externals interface {
	SandboxCreate(context.Context, Job, Action) (Receipt, error)
	RepositoryClone(context.Context, Job, Action) (Receipt, error)
	RouteCreate(context.Context, Job, Action) (Receipt, error)
	AgentInitialTurn(context.Context, Job, Delivery) (string, NativeTurn, error)
	AgentInitialTurns(context.Context, Job) (string, []NativeTurn, error)
	AgentTurns(context.Context, Job, string) ([]NativeTurn, error)
	AgentSubmit(context.Context, Job, Delivery) (NativeTurn, error)
	AgentWait(context.Context, Job, string, string) (NativeTurn, error)
	RouteRevoke(context.Context, Job, Action) (Receipt, error)
	SandboxDelete(context.Context, Job, Action) (Receipt, error)
}

type steeringStore interface {
	BindNativeSteer(context.Context, string, string, string) error
}

type steeringExternals interface {
	AgentSteer(context.Context, Job, Delivery) (string, error)
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

const (
	commandEvidenceProducer = "dorf-go-worker"
	observedProvenance      = "observed"
)

const (
	BarrierBeforeSubmit           = "before-submit"
	BarrierAfterSubmitBeforeBind  = "after-submit-before-bind"
	BarrierNativeActive           = "native-active"
	BarrierSetupComplete          = "setup-complete-before-record"
	BarrierCheckExited            = "check-exited-before-record"
	BarrierPushAccepted           = "push-accepted-before-record"
	BarrierPullRequestAccepted    = "pull-request-accepted-before-record"
	BarrierReviewerRouteRevoked   = "reviewer-route-revoked-before-record"
	BarrierReviewerSandboxDeleted = "reviewer-sandbox-deleted-before-record"
	BarrierMainRouteRevoked       = "main-route-revoked-before-record"
	BarrierMainSandboxDeleted     = "main-sandbox-deleted-before-record"
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
		Provenance      string    `json:"provenance"`
		Command         string    `json:"command"`
		ExitCode        int       `json:"exit_code"`
		StartedAt       time.Time `json:"started_at"`
		FinishedAt      time.Time `json:"finished_at"`
		Stdout          string    `json:"stdout"`
		Stderr          string    `json:"stderr"`
		StdoutTruncated bool      `json:"stdout_truncated"`
		StderrTruncated bool      `json:"stderr_truncated"`
		Redactions      []string  `json:"redactions"`
	}{identity, revision, commandEvidenceProducer, observedProvenance, observation.Command, observation.ExitCode, observation.StartedAt, observation.FinishedAt, string(observation.Stdout), string(observation.Stderr), observation.StdoutCut, observation.StderrCut, observation.Redactions})
}

func (s Service) retainEvidence(ownerID, kind, actionID, checkID, revision string, startedAt, finishedAt time.Time, contents []byte) (Evidence, error) {
	blob, err := s.Evidence.Put(contents)
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{ID: EvidenceID(ownerID, kind), Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Provenance: observedProvenance, Kind: kind, ActionID: actionID, CheckID: checkID, Revision: revision, StartedAt: startedAt.UTC().Truncate(time.Microsecond), FinishedAt: finishedAt.UTC().Truncate(time.Microsecond)}, nil
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

func (s Service) DeliverInitial(ctx context.Context, job Job, session Action, delivery Delivery) (string, error) {
	run := delivery.AgentRun
	if run.SessionID != "" {
		return "", s.Store.UncertainAgentRun(ctx, run.ID, "initial AgentRun is bound while its native Session action is unsettled")
	}
	var sessionID string
	contract := nativeAgentRunContract{
		service:             s,
		delivery:            delivery,
		run:                 run,
		label:               "initial",
		bindUnsupportedTurn: true,
		submitNew: func(ctx context.Context, _ AgentRun) (nativeAgentBinding, error) {
			id, turn, err := s.Externals.AgentInitialTurn(ctx, job, delivery)
			sessionID = id
			return nativeAgentBinding{SessionID: id, Turn: turn}, err
		},
		recover: func(ctx context.Context, _ AgentRun) (nativeAgentBinding, error) {
			id, turns, err := s.Externals.AgentInitialTurns(ctx, job)
			if err != nil || id == "" || len(turns) == 0 {
				return nativeAgentBinding{}, err
			}
			sessionID = id
			return nativeAgentBinding{SessionID: id, Turn: turns[len(turns)-1]}, nil
		},
		bindSession: func(ctx context.Context, binding nativeAgentBinding) error {
			sessionID = binding.SessionID
			return s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: binding.SessionID})
		},
		beforeBind: func(ctx context.Context) error {
			if err := s.requireClaim(ctx); err != nil {
				_ = s.Store.UncertainAction(ctx, session.ID)
				return err
			}
			return nil
		},
		onSubmitError: func(ctx context.Context, run AgentRun, _ nativeAgentBinding, err error) (NativeTurn, error) {
			_ = s.Store.UncertainAction(ctx, session.ID)
			var attention interface{ AttentionNeeded() bool }
			if errors.As(err, &attention) && attention.AttentionNeeded() {
				return NativeTurn{}, s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
			}
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if failErr := s.Store.FailAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return NativeTurn{}, failErr
				}
				return NativeTurn{}, err
			}
			_ = s.Store.AgentRunAttention(ctx, run.ID, "initial native submission is awaiting isolated Session reconciliation: "+err.Error())
			return NativeTurn{}, err
		},
	}
	if _, err := contract.execute(ctx); err != nil {
		return "", err
	}
	return sessionID, nil
}

func (s Service) Deliver(ctx context.Context, job Job, delivery Delivery) (bool, error) {
	if delivery.Message.Intent == MessageSteer && (delivery.AgentRun.NativeTurnID == "" || delivery.AgentRun.NativeTurnID == delivery.Message.TargetTurnID) {
		return s.deliverSteer(ctx, job, delivery)
	}
	return s.deliverTurnStart(ctx, job, delivery)
}

func (s Service) deliverTurnStart(ctx context.Context, job Job, delivery Delivery) (bool, error) {
	run := delivery.AgentRun
	contract := nativeAgentRunContract{
		service:              s,
		delivery:             delivery,
		run:                  run,
		label:                "native",
		reconcileSubmitError: true,
		bindUnsupportedTurn:  true,
		submitBound: func(ctx context.Context, run AgentRun) (nativeAgentBinding, error) {
			delivery.AgentRun = run
			turn, err := s.Externals.AgentSubmit(ctx, job, delivery)
			return nativeAgentBinding{SessionID: run.SessionID, Turn: turn}, err
		},
		history: func(ctx context.Context, run AgentRun) (nativeAgentHistory, error) {
			turns, err := s.Externals.AgentTurns(ctx, job, run.SessionID)
			return nativeAgentHistory{SessionID: run.SessionID, Turns: turns}, err
		},
		wait: func(ctx context.Context, run AgentRun, turnID string) (nativeAgentBinding, error) {
			turn, err := s.Externals.AgentWait(ctx, job, run.SessionID, turnID)
			return nativeAgentBinding{SessionID: run.SessionID, Turn: turn}, err
		},
		validateOwner: func(run AgentRun, _ string, sessionID string) error {
			if sessionID == "" || sessionID != run.SessionID {
				return fmt.Errorf("native recovery conflicts with the bound Session")
			}
			return nil
		},
		beforeBind: s.requireClaim,
		onReadError: func(ctx context.Context, runID string, err error) {
			_ = s.Store.AgentRunAttention(ctx, runID, "native Session or submitted turn is currently unavailable: "+err.Error())
		},
		onSubmitError: func(ctx context.Context, run AgentRun, _ nativeAgentBinding, err error) (NativeTurn, error) {
			var definite interface{ DefiniteNoSubmit() bool }
			if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
				if failErr := s.Store.FailAgentRun(ctx, run.ID, err.Error()); failErr != nil {
					return NativeTurn{}, failErr
				}
			}
			return NativeTurn{}, err
		},
	}
	outcome, err := contract.execute(ctx)
	if err != nil {
		return false, err
	}
	return terminalNative(outcome.Status), nil
}

func (s Service) deliverSteer(ctx context.Context, job Job, delivery Delivery) (bool, error) {
	store, ok := s.Store.(steeringStore)
	if !ok {
		return false, errors.New("store does not support native turn steering")
	}
	externals, ok := s.Externals.(steeringExternals)
	if !ok {
		return false, errors.New("agent harness does not support native turn steering")
	}
	run := delivery.AgentRun
	turns, err := s.Externals.AgentTurns(ctx, job, run.SessionID)
	if err != nil {
		_ = s.Store.AgentRunAttention(ctx, run.ID, "native Session history is currently unavailable: "+err.Error())
		return false, err
	}
	reconciliation := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, turns)
	if reconciliation.Classification == "completed" {
		if err := s.requireClaim(ctx); err != nil {
			return false, err
		}
		return true, store.BindNativeSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciliation.Turn.Status)
	}
	if reconciliation.Classification == "target-terminal" {
		if !run.BaselineRecorded && turns[len(turns)-1].ID != delivery.Message.TargetTurnID {
			return false, s.Store.UncertainAgentRun(ctx, run.ID, "native turns appeared after the terminal steer target before a fallback baseline was recorded")
		}
		return s.deliverTurnStart(ctx, job, delivery)
	}
	if reconciliation.Classification == "uncertain" {
		return false, s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason)
	}
	if !run.BaselineRecorded {
		if err := s.Store.PrepareAgentRun(ctx, run.ID, delivery.Message.TargetTurnID); err != nil {
			return false, err
		}
		delivery.AgentRun.BaselineRecorded = true
		delivery.AgentRun.BaselineTurnID = delivery.Message.TargetTurnID
	}
	if err := s.reach(ctx, BarrierBeforeSubmit, delivery); err != nil {
		return false, err
	}
	if err := s.Store.BeginTurnSubmission(ctx, run.ID); err != nil {
		return false, err
	}
	acceptedTurnID, err := externals.AgentSteer(ctx, job, delivery)
	if err != nil {
		observed, inspectErr := s.Externals.AgentTurns(ctx, job, run.SessionID)
		if inspectErr != nil {
			reason := "native steer acknowledgement is genuinely uncertain: " + err.Error() + "; history inspection failed: " + inspectErr.Error()
			return false, s.Store.UncertainAgentRun(ctx, run.ID, reason)
		}
		reconciled := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, observed)
		if reconciled.Classification == "completed" {
			if claimErr := s.requireClaim(ctx); claimErr != nil {
				return false, claimErr
			}
			return true, store.BindNativeSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciled.Turn.Status)
		}
		if reconciled.Classification == "target-terminal" {
			if !delivery.AgentRun.BaselineRecorded && observed[len(observed)-1].ID != delivery.Message.TargetTurnID {
				return false, s.Store.UncertainAgentRun(ctx, run.ID, "native turns appeared after the terminal steer target before a fallback baseline was recorded")
			}
			return s.deliverTurnStart(ctx, job, delivery)
		}
		if reconciled.Classification == "uncertain" {
			return false, s.Store.UncertainAgentRun(ctx, run.ID, reconciled.Reason)
		}
		return false, err
	}
	if acceptedTurnID != delivery.Message.TargetTurnID {
		return false, s.Store.UncertainAgentRun(ctx, run.ID, "native steer acknowledgement named a different active turn")
	}
	if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
		return false, err
	}
	if err := s.requireClaim(ctx); err != nil {
		return false, err
	}
	return true, store.BindNativeSteer(ctx, run.ID, acceptedTurnID, reconciliation.Turn.Status)
}

func (s Service) reach(ctx context.Context, point string, delivery Delivery) error {
	if s.Barrier == nil {
		return nil
	}
	return s.Barrier.Reach(ctx, point, delivery)
}

func terminalNative(status string) bool {
	return status == "completed" || status == "failed" || status == "interrupted"
}

func activeNative(status string) bool {
	// "inProgress" is the app-server thread/read spelling. "running" is
	// Dorf's local status immediately after turn/start acceptance.
	return status == "running" || status == "inProgress"
}

func (s Service) Cleanup(ctx context.Context, jobID string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error {
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
		if err := s.cleanupStep(ctx, job.ID, "reconciling any unsettled implementation native mutation", func() error { return s.reconcileCleanupMutation(ctx, job) }); err != nil {
			return err
		}
		if reviewStore, ok := s.Store.(ReviewStore); ok {
			runs, err := reviewStore.CleanupReviewRuns(ctx, job.ID)
			if err != nil {
				_ = s.Store.SetCleanupAttention(ctx, job.ID, "enumerating exact recorded reviewer resources: "+err.Error())
				return err
			}
			reviewExternals, externalOK := s.Externals.(ReviewExternals)
			if len(runs) > 0 && !externalOK {
				err := fmt.Errorf("cleanup cannot reconcile persisted review resources without review externals")
				_ = s.Store.SetCleanupAttention(ctx, job.ID, err.Error())
				return err
			}
			for _, run := range runs {
				if run.JobID != job.ID || run.Revision == "" || run.Capability != ReviewReadOnlyCapability || run.ReviewerSandboxID != ReviewSandboxName(run.ID) || len(run.ReviewerOwnerNonce) != 64 {
					err := fmt.Errorf("cleanup cannot reconcile malformed reviewer resource for AgentRun %s", run.ID)
					_ = s.Store.SetCleanupAttention(ctx, job.ID, err.Error())
					return err
				}
				settled := run.State == AgentRunCompleted || run.State == AgentRunFailed || run.State == AgentRunInterrupted
				if !settled {
					if err := reviewStore.InterruptReviewRun(ctx, run.ID, "admission closed; exact isolated reviewer resources are being reclaimed"); err != nil {
						_ = s.Store.SetCleanupAttention(ctx, job.ID, "interrupting unsettled reviewer AgentRun "+run.ID+": "+err.Error())
						return err
					}
					run.State = AgentRunInterrupted
				}
				detail := fmt.Sprintf("reconciling reviewer route %s for AgentRun %s Revision %s", emptyCleanupIdentity(run.ReviewerRouteID), run.ID, run.Revision)
				if err := s.cleanupStep(ctx, job.ID, detail, func() error {
					routeAction, err := reviewStore.BeginReviewRouteCleanup(ctx, run.ID)
					if err != nil || routeAction.State == ActionSucceeded {
						return err
					}
					receipt, err := reviewExternals.ReviewRouteRevoke(ctx, job, run, routeAction)
					if err != nil {
						_ = s.Store.UncertainAction(ctx, routeAction.ID)
						return fmt.Errorf("reconcile exact reviewer route for %s: %w", run.ID, err)
					}
					if err := s.reachWorkflow(ctx, BarrierReviewerRouteRevoked, job.ID, routeAction.ID); err != nil {
						return err
					}
					return s.Store.CompleteAction(ctx, routeAction.ID, receipt)
				}); err != nil {
					return err
				}
				detail = fmt.Sprintf("reconciling reviewer Sandbox %s for AgentRun %s Revision %s", run.ReviewerSandboxID, run.ID, run.Revision)
				if err := s.cleanupStep(ctx, job.ID, detail, func() error {
					sandboxAction, err := reviewStore.BeginReviewSandboxCleanup(ctx, run.ID)
					if err != nil || sandboxAction.State == ActionSucceeded {
						return err
					}
					receipt, err := reviewExternals.ReviewSandboxDelete(ctx, job, run, sandboxAction)
					if err != nil {
						_ = s.Store.UncertainAction(ctx, sandboxAction.ID)
						return fmt.Errorf("reconcile exact reviewer Sandbox %s: %w", run.ReviewerSandboxID, err)
					}
					if err := s.reachWorkflow(ctx, BarrierReviewerSandboxDeleted, job.ID, sandboxAction.ID); err != nil {
						return err
					}
					return s.Store.CompleteAction(ctx, sandboxAction.ID, receipt)
				}); err != nil {
					return err
				}
			}
		}
		for _, step := range []struct {
			kind   ActionKind
			detail string
			owned  bool
		}{
			{ActionRouteRevoke, fmt.Sprintf("reconciling main provider route %s", emptyCleanupIdentity(job.RouteID)), job.RouteID != ""},
			{ActionSandboxDelete, fmt.Sprintf("reconciling main Sandbox %s", emptyCleanupIdentity(job.SandboxID)), job.SandboxID != ""},
		} {
			if !step.owned {
				continue
			}
			if err := s.cleanupStep(ctx, job.ID, step.detail, func() error { _, err := s.reconcile(ctx, job, step.kind); return err }); err != nil {
				return fmt.Errorf("reconcile %s: %w", step.kind, err)
			}
		}
		return s.cleanupStep(ctx, job.ID, "verifying no owned resource or non-cleanup Job claim remains unsettled", func() error { return s.Store.CompleteCleanup(ctx, job.ID) })
	})
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

func (s Service) reconcileCleanupMutation(ctx context.Context, job Job) error {
	delivery, err := s.Store.NativeMutationDelivery(ctx, job.ID)
	if err != nil || delivery == nil {
		return err
	}
	run := delivery.AgentRun
	if run.State == AgentRunUncertain {
		return cleanupBlocked(*delivery, run.Attention)
	}
	sessionID := run.SessionID
	var turns []NativeTurn
	if sessionID == "" {
		sessionID, turns, err = s.Externals.AgentInitialTurns(ctx, job)
		if err == nil && sessionID == "" && len(turns) > 0 {
			err = fmt.Errorf("cleanup initial history returned turns without a native Session")
		}
		if err == nil && sessionID != "" && len(turns) > 0 {
			session, actionErr := s.Store.BeginAction(ctx, job.ID, ActionSessionStart)
			if actionErr != nil {
				return actionErr
			}
			if session.State == ActionSucceeded && session.ExternalID != sessionID {
				err = fmt.Errorf("cleanup isolated Session conflicts with the recorded Session")
			} else if session.State != ActionSucceeded {
				err = s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: sessionID})
			}
			run.SessionID = sessionID
			delivery.AgentRun = run
		}
	} else {
		turns, err = s.Externals.AgentTurns(ctx, job, sessionID)
	}
	if err != nil {
		var attention interface{ AttentionNeeded() bool }
		if errors.As(err, &attention) && attention.AttentionNeeded() {
			if persistErr := s.Store.UncertainAgentRun(ctx, run.ID, err.Error()); persistErr != nil {
				return persistErr
			}
			return cleanupBlocked(*delivery, err.Error())
		}
		reason := "cleanup could not inspect the bound native Session: " + err.Error()
		_ = s.Store.AgentRunAttention(ctx, run.ID, reason)
		return cleanupBlocked(*delivery, reason)
	}
	if delivery.Message.Intent == MessageSteer && (run.NativeTurnID == "" || run.NativeTurnID == delivery.Message.TargetTurnID) {
		reconciliation := ReconcileSteer(run.ID, delivery.Message.TargetTurnID, turns)
		switch reconciliation.Classification {
		case "completed":
			store, ok := s.Store.(steeringStore)
			if !ok {
				return cleanupBlocked(*delivery, "store does not support native turn steering")
			}
			return store.BindNativeSteer(ctx, run.ID, delivery.Message.TargetTurnID, reconciliation.Turn.Status)
		case "no-submit":
			return s.Store.FailAgentRun(ctx, run.ID, "cleanup closed steer delivery after native history proved it was not accepted")
		case "target-terminal":
			if !run.BaselineRecorded {
				return s.Store.FailAgentRun(ctx, run.ID, "cleanup closed steer delivery after native history proved it was not accepted")
			}
		default:
			if err := s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason); err != nil {
				return err
			}
			return cleanupBlocked(*delivery, reconciliation.Reason)
		}
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.NativeTurnID, turns)
	switch reconciliation.Classification {
	case "no-submit":
		reason := "cleanup closed delivery after native history proved no turn was submitted"
		if err := s.Store.FailAgentRun(ctx, run.ID, reason); err != nil {
			return err
		}
		return nil
	case "uncertain":
		if reconciliation.Turn.ID != "" {
			if err := s.Store.BindNativeTurn(ctx, run.ID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
				return err
			}
		} else if err := s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason); err != nil {
			return err
		}
		return cleanupBlocked(*delivery, reconciliation.Reason)
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
		return err
	}
	if reconciliation.Classification != "active" {
		return nil
	}
	outcome, err := s.Externals.AgentWait(ctx, job, sessionID, reconciliation.Turn.ID)
	if err != nil {
		reason := "cleanup is waiting for the exact native turn outcome: " + err.Error()
		_ = s.Store.AgentRunAttention(ctx, run.ID, reason)
		return cleanupBlocked(*delivery, reason)
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, outcome.ID, outcome.Status); err != nil {
		return err
	}
	if !terminalNative(outcome.Status) {
		reason := fmt.Sprintf("cleanup inspection returned nonterminal native status %q", outcome.Status)
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

func (s Service) reconcile(ctx context.Context, job Job, kind ActionKind) (Receipt, error) {
	action, err := s.Store.BeginAction(ctx, job.ID, kind)
	if err != nil {
		return Receipt{}, err
	}
	if action.State == ActionSucceeded {
		return Receipt{ExternalID: action.ExternalID, Outcome: action.Outcome}, nil
	}
	return s.ExecuteAction(ctx, job, action)
}

// ExecuteAction reconciles one already-reserved external mutation. The Action
// identity comes from PostgreSQL; the caller gives the operation its durable
// Absurd Step identity.
func (s Service) ExecuteAction(ctx context.Context, job Job, action Action) (Receipt, error) {
	var receipt Receipt
	var err error
	switch action.Kind {
	case ActionSandboxCreate:
		receipt, err = s.Externals.SandboxCreate(ctx, job, action)
	case ActionRepositoryClone:
		receipt, err = s.Externals.RepositoryClone(ctx, job, action)
	case ActionRouteCreate:
		receipt, err = s.Externals.RouteCreate(ctx, job, action)
	case ActionRouteRevoke:
		receipt, err = s.Externals.RouteRevoke(ctx, job, action)
	case ActionSandboxDelete:
		receipt, err = s.Externals.SandboxDelete(ctx, job, action)
	default:
		err = fmt.Errorf("unsupported Action kind %q", action.Kind)
	}
	if err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		return Receipt{}, err
	}
	point := ""
	if action.Kind == ActionRouteRevoke {
		point = BarrierMainRouteRevoked
	} else if action.Kind == ActionSandboxDelete {
		point = BarrierMainSandboxDeleted
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
	if err := s.Store.CompleteAction(ctx, action.ID, receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
