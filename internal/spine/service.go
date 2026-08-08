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
	StartRun(context.Context, string) error
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

type CodingStore interface {
	BeginCommit(context.Context, string, string) (Action, bool, error)
	RecordSetup(context.Context, string, Evidence, CommandObservation, []DeclaredCheck) error
	RecordRevision(context.Context, string, CommitObservation, Evidence) error
	BeginCheck(context.Context, string, string, string, string) (Check, error)
	RecordCheck(context.Context, Check, Evidence, CommandObservation) error
	AdmitRepair(context.Context, Check) (Message, bool, error)
	MarkReady(context.Context, string, string, []string) error
	BlockWorkflow(context.Context, string, string) error
	DeclaredChecks(context.Context, string) ([]DeclaredCheck, error)
	Checks(context.Context, string) ([]Check, error)
	Evidence(context.Context, string) ([]Evidence, error)
}

type RepositoryExternals interface {
	RepositorySetup(context.Context, Job, Action) (CommandObservation, []DeclaredCheck, error)
	RepositoryCommit(context.Context, Job, Action) (CommitObservation, []byte, error)
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
	BarrierBeforeSubmit          = "before-submit"
	BarrierAfterSubmitBeforeBind = "after-submit-before-bind"
	BarrierNativeActive          = "native-active"
	BarrierSetupComplete         = "setup-complete-before-record"
	BarrierCommitCreated         = "commit-created-before-record"
	BarrierCheckExited           = "check-exited-before-record"
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
}

func (s Service) RunUntilIdle(ctx context.Context, jobID string) (RunDisposition, error) {
	disposition := RunIdle
	err := s.Store.WithJobFence(ctx, jobID, func() error {
		var err error
		disposition, err = s.runFenced(ctx, jobID)
		return err
	})
	return disposition, err
}

func (s Service) Run(ctx context.Context, jobID string) error {
	_, err := s.RunUntilIdle(ctx, jobID)
	return err
}

func (s Service) runFenced(ctx context.Context, jobID string) (RunDisposition, error) {
	job, err := s.Store.Job(ctx, jobID)
	if err != nil {
		return RunIdle, err
	}
	if !job.AdmissionOpen {
		return RunClosed, nil
	}
	if err := s.Store.StartRun(ctx, jobID); err != nil {
		return RunIdle, err
	}
	for _, kind := range []ActionKind{ActionSandboxCreate, ActionRepositoryClone, ActionRouteCreate} {
		if kind == ActionRouteCreate && s.Repository != nil {
			job, err = s.Store.Job(ctx, jobID)
			if err != nil {
				return RunIdle, err
			}
			disposition, err := s.setup(ctx, job)
			if err != nil || disposition == RunBlocked {
				return disposition, err
			}
		}
		job, err = s.Store.Job(ctx, jobID)
		if err != nil {
			return RunIdle, err
		}
		if _, err := s.reconcile(ctx, job, kind); err != nil {
			return RunIdle, fmt.Errorf("reconcile %s: %w", kind, err)
		}
	}
	job, err = s.Store.Job(ctx, jobID)
	if err != nil {
		return RunIdle, err
	}
	sessionID := job.SessionID
	if !reviewPhasePreemptsFIFO(job.WorkflowPhase) {
		session, err := s.Store.BeginAction(ctx, job.ID, ActionSessionStart)
		if err != nil {
			return RunIdle, err
		}
		sessionID = session.ExternalID
		if session.State != ActionSucceeded {
			delivery, err := s.Store.NextDelivery(ctx, jobID, "")
			if err != nil {
				return RunIdle, err
			}
			if delivery == nil || delivery.Message.Sequence != 1 && delivery.Message.RetryOfMessageID == "" {
				return RunIdle, fmt.Errorf("unbound native Session has no initial delivery")
			}
			switch delivery.AgentRun.State {
			case AgentRunFailed, AgentRunInterrupted, AgentRunUncertain:
				return RunBlocked, nil
			}
			sessionID, err = s.deliverInitial(ctx, job, session, *delivery)
			if err != nil {
				return RunIdle, fmt.Errorf("reconcile initial native Session and turn: %w", err)
			}
		}
	}
	for {
		job, err = s.Store.Job(ctx, jobID)
		if err != nil {
			return RunIdle, err
		}
		if !job.AdmissionOpen {
			return RunClosed, nil
		}
		if reviewPhasePreemptsFIFO(job.WorkflowPhase) {
			disposition, progressed, err := s.advanceCoding(ctx, job)
			if err != nil || disposition == RunBlocked || !progressed {
				return disposition, err
			}
			continue
		}
		delivery, err := s.Store.NextDelivery(ctx, jobID, sessionID)
		if err != nil {
			return RunIdle, err
		}
		if delivery == nil {
			if s.Repository == nil {
				return RunIdle, nil
			}
			disposition, progressed, err := s.advanceCoding(ctx, job)
			if err != nil || disposition == RunBlocked || !progressed {
				return disposition, err
			}
			continue
		}
		switch delivery.AgentRun.State {
		case AgentRunFailed, AgentRunInterrupted, AgentRunUncertain:
			return RunBlocked, nil
		}
		if err := s.deliver(ctx, job, *delivery); err != nil {
			return RunIdle, err
		}
	}
}

func reviewPhasePreemptsFIFO(phase string) bool {
	switch phase {
	case "review-planning", "review-triage", "reviewing":
		return true
	default:
		return false
	}
}

func (s Service) setup(ctx context.Context, job Job) (RunDisposition, error) {
	store := s.Store.(CodingStore)
	action, err := s.Store.BeginSetup(ctx, job.ID)
	if err != nil {
		return RunIdle, err
	}
	if action.State == ActionSucceeded {
		return RunIdle, nil
	}
	if action.State == ActionFailed {
		return RunBlocked, nil
	}
	observation, declared, err := s.Repository.RepositorySetup(ctx, job, action)
	if err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		if attentionNeeded(err) {
			_ = store.BlockWorkflow(ctx, job.ID, err.Error())
			return RunBlocked, nil
		}
		return RunIdle, err
	}
	observation = canonicalCommandObservation(observation)
	artifact, err := commandArtifact(action.ID, job.StartingRevision, observation)
	if err != nil {
		return RunIdle, err
	}
	evidenceRecord, err := s.retainEvidence(action.ID, "repository-setup", action.ID, "", job.StartingRevision, observation.StartedAt, observation.FinishedAt, artifact)
	if err != nil {
		return RunIdle, err
	}
	if err := s.reachWorkflow(ctx, BarrierSetupComplete, job.ID, action.ID); err != nil {
		return RunIdle, err
	}
	if err := store.RecordSetup(ctx, action.ID, evidenceRecord, observation, declared); err != nil {
		return RunIdle, err
	}
	if observation.ExitCode != 0 {
		return RunBlocked, nil
	}
	return RunIdle, nil
}

func (s Service) advanceCoding(ctx context.Context, job Job) (RunDisposition, bool, error) {
	store := s.Store.(CodingStore)
	switch job.WorkflowPhase {
	case "ready":
		return RunIdle, false, nil
	case "blocked":
		return RunBlocked, false, nil
	case "implementing", "repairing", "review-repairing", "committing":
		if job.WorkflowPhase == "review-repairing" {
			observer, ok := s.Externals.(ReviewAdjudicationExternals)
			if !ok {
				return RunIdle, false, fmt.Errorf("review adjudication requires an observed Git workspace boundary")
			}
			changed, err := observer.RepositoryHasChanges(ctx, job)
			if err != nil {
				return RunIdle, false, err
			}
			if !changed {
				reviewStore := s.Store.(ReviewStore)
				if err := reviewStore.RejectReviewFinding(ctx, job.ID, job.Revision); err != nil {
					return RunIdle, false, err
				}
				return RunIdle, true, nil
			}
		}
		parent := job.Revision
		action, started, err := store.BeginCommit(ctx, job.ID, parent)
		if err != nil {
			return RunIdle, false, err
		}
		if !started {
			return RunIdle, true, nil
		}
		if action.State != ActionSucceeded {
			observation, artifact, err := s.Repository.RepositoryCommit(ctx, job, action)
			if err != nil {
				_ = s.Store.UncertainAction(ctx, action.ID)
				if attentionNeeded(err) {
					_ = store.BlockWorkflow(ctx, job.ID, err.Error())
					return RunBlocked, false, nil
				}
				return RunIdle, false, err
			}
			evidenceRecord, err := s.retainEvidence(action.ID, "git-revision", action.ID, "", observation.Revision, observation.StartedAt, observation.FinishedAt, artifact)
			if err != nil {
				return RunIdle, false, err
			}
			if err := s.reachWorkflow(ctx, BarrierCommitCreated, job.ID, action.ID); err != nil {
				return RunIdle, false, err
			}
			if err := store.RecordRevision(ctx, action.ID, observation, evidenceRecord); err != nil {
				return RunIdle, false, err
			}
		}
		return RunIdle, true, nil
	case "checking":
		declared, err := store.DeclaredChecks(ctx, job.ID)
		if err != nil {
			if attentionNeeded(err) {
				_ = store.BlockWorkflow(ctx, job.ID, err.Error())
				return RunBlocked, false, nil
			}
			return RunIdle, false, err
		}
		for _, declaration := range declared {
			check, err := store.BeginCheck(ctx, job.ID, job.Revision, declaration.Name, declaration.Command)
			if err != nil {
				return RunIdle, false, err
			}
			if check.State == "passed" {
				continue
			}
			if check.State == "failed" {
				return s.handleFailedCheck(ctx, job, check)
			}
			observation, err := s.Repository.RepositoryCheck(ctx, job, check)
			if err != nil {
				if attentionNeeded(err) {
					_ = store.BlockWorkflow(ctx, job.ID, err.Error())
					return RunBlocked, false, nil
				}
				return RunIdle, false, err
			}
			observation = canonicalCommandObservation(observation)
			artifact, err := commandArtifact(check.ID, job.Revision, observation)
			if err != nil {
				return RunIdle, false, err
			}
			evidenceRecord, err := s.retainEvidence(check.ID, "check-output", "", check.ID, job.Revision, observation.StartedAt, observation.FinishedAt, artifact)
			if err != nil {
				return RunIdle, false, err
			}
			if err := s.reachWorkflow(ctx, BarrierCheckExited, job.ID, check.ID); err != nil {
				return RunIdle, false, err
			}
			if err := store.RecordCheck(ctx, check, evidenceRecord, observation); err != nil {
				return RunIdle, false, err
			}
			check.State, check.ExitCode, check.EvidenceID, check.EvidenceDigest = "passed", observation.ExitCode, evidenceRecord.ID, evidenceRecord.Digest
			if observation.ExitCode != 0 {
				check.State = "failed"
				return s.handleFailedCheck(ctx, job, check)
			}
		}
		checks, err := store.Checks(ctx, job.ID)
		if err != nil {
			return RunIdle, false, err
		}
		records, err := store.Evidence(ctx, job.ID)
		if err != nil {
			return RunIdle, false, err
		}
		verified, err := VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, s.Evidence)
		if err != nil {
			reason := fmt.Sprintf("Revision %s Evidence verification failed: %v", job.Revision, err)
			if blockErr := store.BlockWorkflow(ctx, job.ID, reason); blockErr != nil {
				return RunIdle, false, blockErr
			}
			return RunBlocked, false, nil
		}
		verifiedIDs := make([]string, 0, len(verified))
		for _, result := range verified {
			verifiedIDs = append(verifiedIDs, result.EvidenceID)
		}
		if reviewStore, ok := s.Store.(ReviewStore); ok {
			if err := reviewStore.MarkChecksVerified(ctx, job.ID, job.Revision, verifiedIDs); err != nil {
				return RunIdle, false, err
			}
		} else if err := store.MarkReady(ctx, job.ID, job.Revision, verifiedIDs); err != nil {
			return RunIdle, false, err
		}
		return RunIdle, true, nil
	case "review-planning", "review-triage", "reviewing":
		return s.advanceReview(ctx, job)
	case "review-activation":
		// Exact-Revision Checks are proven. The orchestrator must now bind the
		// implementation-requested allowlisted Roles, including an explicit
		// empty set, before the atomic policy result can be computed.
		return RunIdle, false, nil
	default:
		return RunIdle, false, fmt.Errorf("unsupported coding workflow phase %q", job.WorkflowPhase)
	}
}

func attentionNeeded(err error) bool {
	var attention interface{ AttentionNeeded() bool }
	return errors.As(err, &attention) && attention.AttentionNeeded()
}

func (s Service) handleFailedCheck(ctx context.Context, job Job, check Check) (RunDisposition, bool, error) {
	store := s.Store.(CodingStore)
	if job.RepairCount == 0 && job.ReviewRepairCount == 0 {
		if _, _, err := store.AdmitRepair(ctx, check); err != nil {
			return RunIdle, false, err
		}
		return RunIdle, true, nil
	}
	reason := fmt.Sprintf("Check %s still failed at Revision %s with exit %d", check.Name, check.Revision, check.ExitCode)
	if err := store.BlockWorkflow(ctx, job.ID, reason); err != nil {
		return RunIdle, false, err
	}
	return RunBlocked, false, nil
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

func (s Service) deliverInitial(ctx context.Context, job Job, session Action, delivery Delivery) (string, error) {
	run := delivery.AgentRun
	if run.SessionID != "" {
		return "", s.Store.UncertainAgentRun(ctx, run.ID, "initial AgentRun is bound while its native Session action is unsettled")
	}
	if !run.BaselineRecorded {
		if err := s.Store.PrepareAgentRun(ctx, run.ID, ""); err != nil {
			return "", err
		}
		run.BaselineRecorded, run.State = true, AgentRunSubmitting
		delivery.AgentRun = run
	} else if run.BaselineTurnID != "" {
		return "", s.Store.UncertainAgentRun(ctx, run.ID, "initial AgentRun has a nonempty native baseline")
	}
	if err := s.reach(ctx, BarrierBeforeSubmit, delivery); err != nil {
		return "", err
	}
	if err := s.Store.BeginTurnSubmission(ctx, run.ID); err != nil {
		return "", err
	}
	sessionID, turn, err := s.Externals.AgentInitialTurn(ctx, job, delivery)
	if err != nil {
		var attention interface{ AttentionNeeded() bool }
		if errors.As(err, &attention) && attention.AttentionNeeded() {
			_ = s.Store.UncertainAction(ctx, session.ID)
			return "", s.Store.UncertainAgentRun(ctx, run.ID, err.Error())
		}
		var definite interface{ DefiniteNoSubmit() bool }
		if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
			_ = s.Store.UncertainAction(ctx, session.ID)
			return "", s.Store.FailAgentRun(ctx, run.ID, err.Error())
		}
		_ = s.Store.UncertainAction(ctx, session.ID)
		_ = s.Store.AgentRunAttention(ctx, run.ID, "initial native submission is awaiting isolated Session reconciliation: "+err.Error())
		return "", err
	}
	if sessionID == "" || turn.ID == "" {
		_ = s.Store.UncertainAction(ctx, session.ID)
		return "", s.Store.UncertainAgentRun(ctx, run.ID, "initial native submission returned an incomplete Session or turn binding")
	}
	if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
		return "", err
	}
	if err := s.Store.CompleteAction(ctx, session.ID, Receipt{ExternalID: sessionID}); err != nil {
		return "", err
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, turn.ID, turn.Status); err != nil {
		return "", err
	}
	return sessionID, nil
}

func (s Service) deliver(ctx context.Context, job Job, delivery Delivery) error {
	run := delivery.AgentRun
	turns, err := s.Externals.AgentTurns(ctx, job, run.SessionID)
	if err != nil {
		_ = s.Store.AgentRunAttention(ctx, run.ID, "native Session history is currently unavailable: "+err.Error())
		return err
	}
	if !run.BaselineRecorded {
		baseline := ""
		if len(turns) > 0 {
			baseline = turns[len(turns)-1].ID
		}
		if err := s.Store.PrepareAgentRun(ctx, run.ID, baseline); err != nil {
			return err
		}
		run.BaselineRecorded, run.BaselineTurnID, run.State = true, baseline, AgentRunSubmitting
		delivery.AgentRun = run
	}
	reconciliation := ReconcileTurns(run.BaselineRecorded, run.BaselineTurnID, run.NativeTurnID, turns)
	if reconciliation.Classification == "uncertain" {
		if reconciliation.Turn.ID != "" {
			return s.Store.BindNativeTurn(ctx, run.ID, reconciliation.Turn.ID, reconciliation.Turn.Status)
		}
		return s.Store.UncertainAgentRun(ctx, run.ID, reconciliation.Reason)
	}
	if reconciliation.Classification == "no-submit" {
		return s.submit(ctx, job, delivery)
	}
	if err := s.Store.BindNativeTurn(ctx, run.ID, reconciliation.Turn.ID, reconciliation.Turn.Status); err != nil {
		return err
	}
	if reconciliation.Classification == "active" {
		if err := s.reach(ctx, BarrierNativeActive, delivery); err != nil {
			return err
		}
		outcome, err := s.Externals.AgentWait(ctx, job, run.SessionID, reconciliation.Turn.ID)
		if err != nil {
			_ = s.Store.AgentRunAttention(ctx, run.ID, "submitted native turn outcome is currently unavailable: "+err.Error())
			return err
		}
		return s.Store.BindNativeTurn(ctx, run.ID, outcome.ID, outcome.Status)
	}
	return nil
}

func (s Service) submit(ctx context.Context, job Job, delivery Delivery) error {
	if err := s.reach(ctx, BarrierBeforeSubmit, delivery); err != nil {
		return err
	}
	if err := s.Store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		return err
	}
	turn, err := s.Externals.AgentSubmit(ctx, job, delivery)
	if err != nil {
		var definite interface{ DefiniteNoSubmit() bool }
		if errors.As(err, &definite) && definite.DefiniteNoSubmit() {
			return s.Store.FailAgentRun(ctx, delivery.AgentRun.ID, err.Error())
		}
		turns, inspectErr := s.Externals.AgentTurns(ctx, job, delivery.AgentRun.SessionID)
		if inspectErr != nil {
			reason := "native submission is genuinely uncertain: " + err.Error() + "; history inspection failed: " + inspectErr.Error()
			return s.Store.UncertainAgentRun(ctx, delivery.AgentRun.ID, reason)
		}
		reconciliation := ReconcileTurns(true, delivery.AgentRun.BaselineTurnID, "", turns)
		if reconciliation.Classification == "no-submit" {
			return err
		}
		if reconciliation.Classification == "uncertain" {
			if reconciliation.Turn.ID != "" {
				return s.Store.BindNativeTurn(ctx, delivery.AgentRun.ID, reconciliation.Turn.ID, reconciliation.Turn.Status)
			}
			return s.Store.UncertainAgentRun(ctx, delivery.AgentRun.ID, reconciliation.Reason)
		}
		turn = reconciliation.Turn
	}
	if err := s.reach(ctx, BarrierAfterSubmitBeforeBind, delivery); err != nil {
		return err
	}
	if err := s.Store.BindNativeTurn(ctx, delivery.AgentRun.ID, turn.ID, turn.Status); err != nil {
		return err
	}
	if terminalNative(turn.Status) {
		return nil
	}
	if !activeNative(turn.Status) {
		return nil
	}
	if err := s.reach(ctx, BarrierNativeActive, delivery); err != nil {
		return err
	}
	outcome, err := s.Externals.AgentWait(ctx, job, delivery.AgentRun.SessionID, turn.ID)
	if err != nil {
		_ = s.Store.AgentRunAttention(ctx, delivery.AgentRun.ID, "submitted native turn outcome is currently unavailable: "+err.Error())
		return err
	}
	return s.Store.BindNativeTurn(ctx, delivery.AgentRun.ID, outcome.ID, outcome.Status)
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
		if err := s.reconcileCleanupMutation(ctx, job); err != nil {
			return err
		}
		if reviewStore, ok := s.Store.(ReviewStore); ok {
			runs, err := reviewStore.CleanupReviewRuns(ctx, job.ID)
			if err != nil {
				return err
			}
			reviewExternals, externalOK := s.Externals.(ReviewExternals)
			if len(runs) > 0 && !externalOK {
				return fmt.Errorf("cleanup cannot reconcile persisted review resources without review externals")
			}
			for _, run := range runs {
				if run.JobID != job.ID || run.Revision == "" || run.Capability != ReviewReadOnlyCapability || run.ReviewerSandboxID != ReviewSandboxName(run.ID) || len(run.ReviewerOwnerNonce) != 64 {
					return fmt.Errorf("cleanup cannot reconcile malformed reviewer resource for AgentRun %s", run.ID)
				}
				settled := run.State == AgentRunCompleted || run.State == AgentRunFailed || run.State == AgentRunInterrupted
				if !settled {
					if err := reviewStore.InterruptReviewRun(ctx, run.ID, "admission closed; exact isolated reviewer resources are being reclaimed"); err != nil {
						return err
					}
					run.State = AgentRunInterrupted
				}
				routeAction, err := reviewStore.BeginReviewRouteCleanup(ctx, run.ID)
				if err != nil {
					return err
				}
				if routeAction.State != ActionSucceeded {
					receipt, err := reviewExternals.ReviewRouteRevoke(ctx, job, run.AgentRun, routeAction)
					if err != nil {
						_ = s.Store.UncertainAction(ctx, routeAction.ID)
						return fmt.Errorf("reconcile exact reviewer route for %s: %w", run.ID, err)
					}
					if err := s.Store.CompleteAction(ctx, routeAction.ID, receipt); err != nil {
						return err
					}
				}
				sandboxAction, err := reviewStore.BeginReviewSandboxCleanup(ctx, run.ID)
				if err != nil {
					return err
				}
				if sandboxAction.State != ActionSucceeded {
					receipt, err := reviewExternals.ReviewSandboxDelete(ctx, job, run.AgentRun, sandboxAction)
					if err != nil {
						_ = s.Store.UncertainAction(ctx, sandboxAction.ID)
						return fmt.Errorf("reconcile exact reviewer Sandbox %s: %w", run.ReviewerSandboxID, err)
					}
					if err := s.Store.CompleteAction(ctx, sandboxAction.ID, receipt); err != nil {
						return err
					}
				}
			}
		}
		for _, kind := range []ActionKind{ActionRouteRevoke, ActionSandboxDelete} {
			if _, err := s.reconcile(ctx, job, kind); err != nil {
				return fmt.Errorf("reconcile %s: %w", kind, err)
			}
		}
		return s.Store.CompleteCleanup(ctx, job.ID)
	})
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
	var receipt Receipt
	switch kind {
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
		err = fmt.Errorf("unsupported Action kind %q", kind)
	}
	if err != nil {
		_ = s.Store.UncertainAction(ctx, action.ID)
		return Receipt{}, err
	}
	if err := s.Store.CompleteAction(ctx, action.ID, receipt); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}
