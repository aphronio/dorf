package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aphronio/dorf/internal/spine"
)

type MessageResolutionInput struct {
	JobID     string
	MessageID string
	Decision  spine.MessageResolutionDecision
	Authority string
	Reason    string
}

func normalizeResolutionInput(input MessageResolutionInput) (MessageResolutionInput, error) {
	input.JobID = strings.TrimSpace(input.JobID)
	input.MessageID = strings.TrimSpace(input.MessageID)
	if input.JobID == "" || input.MessageID == "" || strings.TrimSpace(input.Authority) == "" || strings.TrimSpace(input.Reason) == "" {
		return MessageResolutionInput{}, fmt.Errorf("message resolution requires exact Job/message, authority, and complete reason")
	}
	if len(input.Authority) > 1024 || len(input.Reason) > 1<<20 {
		return MessageResolutionInput{}, fmt.Errorf("message resolution authority exceeds 1 KiB or reason exceeds 1 MiB")
	}
	switch input.Decision {
	case spine.ResolutionRetry, spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon:
	default:
		return MessageResolutionInput{}, fmt.Errorf("invalid message resolution decision %q", input.Decision)
	}
	return input, nil
}

func (s Store) DiagnoseMessage(ctx context.Context, jobID, messageID string) (spine.MessageResolutionDiagnosis, error) {
	jobID, messageID = strings.TrimSpace(jobID), strings.TrimSpace(messageID)
	if jobID == "" || messageID == "" {
		return spine.MessageResolutionDiagnosis{}, fmt.Errorf("message diagnosis requires exact Job and message IDs")
	}
	return scanMessageDiagnosis(s.DB.QueryRowContext(ctx, messageDiagnosisSQL, jobID, messageID))
}

const messageDiagnosisSQL = `
	select m.id,m.job_id,m.caller_id,m.sequence,m.input,
	       ar.id,ar.job_id,ar.message_id,ar.action_id,coalesce(ar.session_id,''),ar.state,
	       ar.baseline_native_turn_id is not null,coalesce(ar.baseline_native_turn_id,''),
	       coalesce(ar.native_turn_id,''),coalesce(ar.native_outcome,''),coalesce(ar.attention,''),ar.role,
	       a.id,a.kind,a.state,coalesce(a.external_outcome,''),
	       coalesce(mr.id,''),coalesce(mr.decision,''),coalesce(mr.authority,''),coalesce(mr.reason,''),
	       coalesce(mr.reserved_wake_sequence,0),coalesce(mr.resolved_at,'epoch')
	from dorf.job_messages m
	join dorf.agent_runs ar on ar.message_id=m.id
	join dorf.actions a on a.id=ar.action_id
	left join dorf.message_resolutions mr on mr.job_id=m.job_id and mr.message_id=m.id
	where m.job_id=$1 and m.id=$2`

func scanMessageDiagnosis(row rowScanner) (spine.MessageResolutionDiagnosis, error) {
	var diagnosis spine.MessageResolutionDiagnosis
	var resolution spine.MessageResolution
	err := row.Scan(
		&diagnosis.Message.ID, &diagnosis.Message.JobID, &diagnosis.Message.CallerID, &diagnosis.Message.Sequence, &diagnosis.Message.Input,
		&diagnosis.AgentRun.ID, &diagnosis.AgentRun.JobID, &diagnosis.AgentRun.MessageID, &diagnosis.AgentRun.ActionID,
		&diagnosis.AgentRun.SessionID, &diagnosis.AgentRun.State, &diagnosis.AgentRun.BaselineRecorded,
		&diagnosis.AgentRun.BaselineTurnID, &diagnosis.AgentRun.NativeTurnID, &diagnosis.AgentRun.NativeOutcome,
		&diagnosis.AgentRun.Attention, &diagnosis.AgentRun.Role,
		&diagnosis.Action.ID, &diagnosis.Action.Kind, &diagnosis.Action.State, &diagnosis.Action.Outcome,
		&resolution.ID, &resolution.Decision, &resolution.Authority, &resolution.Reason,
		&resolution.ReservedWakeSequence, &resolution.ResolvedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return diagnosis, ErrNotFound
	}
	if err != nil {
		return diagnosis, err
	}
	diagnosis.JobID = diagnosis.Message.JobID
	diagnosis.NativeSessionID = diagnosis.AgentRun.SessionID
	diagnosis.NativeTurnID = diagnosis.AgentRun.NativeTurnID
	diagnosis.NativeOutcome = diagnosis.AgentRun.NativeOutcome
	if resolution.ID != "" {
		resolution.JobID, resolution.MessageID = diagnosis.JobID, diagnosis.Message.ID
		diagnosis.Resolution = &resolution
		underlying, _ := diagnoseUnresolved(diagnosis.AgentRun)
		diagnosis.ReconciliationReason = underlying + "; input has an append-only " + string(resolution.Decision) + " resolution receipt"
		diagnosis.SafeDecisions = []spine.MessageResolutionDecision{}
		return diagnosis, nil
	}
	diagnosis.ReconciliationReason, diagnosis.SafeDecisions = diagnoseUnresolved(diagnosis.AgentRun)
	return diagnosis, nil
}

func diagnoseUnresolved(run spine.AgentRun) (string, []spine.MessageResolutionDecision) {
	abandon := []spine.MessageResolutionDecision{spine.ResolutionAbandon}
	switch run.State {
	case spine.AgentRunFailed:
		if run.BaselineRecorded && run.NativeTurnID == "" && run.NativeOutcome == "" {
			return "native non-delivery is positively proven: " + nonemptyReason(run.Attention), []spine.MessageResolutionDecision{spine.ResolutionRetry, spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon}
		}
		return "native turn failed and may have mutated the workspace: " + nonemptyReason(run.Attention), []spine.MessageResolutionDecision{spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon}
	case spine.AgentRunInterrupted:
		return "native turn was interrupted and may have mutated the workspace: " + nonemptyReason(run.Attention), []spine.MessageResolutionDecision{spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon}
	case spine.AgentRunUncertain:
		return "native delivery is ambiguous: " + nonemptyReason(run.Attention), []spine.MessageResolutionDecision{spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon}
	case spine.AgentRunCompleted:
		return "native turn completed; the input is already settled", []spine.MessageResolutionDecision{}
	case spine.AgentRunPending, spine.AgentRunSubmitting, spine.AgentRunActive:
		return "delivery is not terminal: " + string(run.State), abandon
	default:
		return "delivery has no durable terminal classification", abandon
	}
}

func nonemptyReason(reason string) string {
	if reason == "" {
		return "durable reconciliation recorded no additional detail"
	}
	return reason
}

func (s Store) PlanMessageResolution(ctx context.Context, input MessageResolutionInput) (spine.MessageResolutionDiagnosis, spine.MessageResolution, error) {
	input, err := normalizeResolutionInput(input)
	if err != nil {
		return spine.MessageResolutionDiagnosis{}, spine.MessageResolution{}, err
	}
	diagnosis, err := s.DiagnoseMessage(ctx, input.JobID, input.MessageID)
	if err != nil {
		return diagnosis, spine.MessageResolution{}, err
	}
	if diagnosis.Resolution != nil {
		if resolutionMatches(*diagnosis.Resolution, input) {
			return diagnosis, *diagnosis.Resolution, nil
		}
		return diagnosis, spine.MessageResolution{}, fmt.Errorf("message %s already has a conflicting immutable resolution receipt", input.MessageID)
	}
	if !slices.Contains(diagnosis.SafeDecisions, input.Decision) {
		return diagnosis, spine.MessageResolution{}, fmt.Errorf("decision %q is not proven safe: %s", input.Decision, diagnosis.ReconciliationReason)
	}
	return diagnosis, spine.MessageResolution{ID: spine.MessageResolutionID(input.JobID, input.MessageID), JobID: input.JobID, MessageID: input.MessageID, Decision: input.Decision, Authority: input.Authority, Reason: input.Reason}, nil
}

func (s Store) ResolveMessage(ctx context.Context, input MessageResolutionInput) (spine.MessageResolution, bool, error) {
	input, err := normalizeResolutionInput(input)
	if err != nil {
		return spine.MessageResolution{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.MessageResolution{}, false, err
	}
	defer tx.Rollback()
	var admissionOpen bool
	if err := tx.QueryRowContext(ctx, `select admission_open from dorf.jobs where id=$1 for update`, input.JobID).Scan(&admissionOpen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spine.MessageResolution{}, false, ErrNotFound
		}
		return spine.MessageResolution{}, false, err
	}
	diagnosis, err := scanMessageDiagnosis(tx.QueryRowContext(ctx, messageDiagnosisSQL, input.JobID, input.MessageID))
	if err != nil {
		return spine.MessageResolution{}, false, err
	}
	if diagnosis.Resolution != nil {
		if !resolutionMatches(*diagnosis.Resolution, input) {
			return spine.MessageResolution{}, false, fmt.Errorf("message %s already has a conflicting immutable resolution receipt", input.MessageID)
		}
		return *diagnosis.Resolution, false, tx.Commit()
	}
	if !admissionOpen {
		return spine.MessageResolution{}, false, fmt.Errorf("Job %s admission is already closed without this message resolution", input.JobID)
	}
	if !slices.Contains(diagnosis.SafeDecisions, input.Decision) {
		return spine.MessageResolution{}, false, fmt.Errorf("decision %q is not proven safe: %s", input.Decision, diagnosis.ReconciliationReason)
	}
	resolution := spine.MessageResolution{ID: spine.MessageResolutionID(input.JobID, input.MessageID), JobID: input.JobID, MessageID: input.MessageID, Decision: input.Decision, Authority: input.Authority, Reason: input.Reason}
	if input.Decision != spine.ResolutionAbandon {
		resolution.ReservedWakeSequence, err = allocateWakeSequenceTx(ctx, tx, input.JobID)
		if err != nil {
			return spine.MessageResolution{}, false, err
		}
	}
	if err := tx.QueryRowContext(ctx, `insert into dorf.message_resolutions(id,job_id,message_id,decision,authority,reason,reserved_wake_sequence) values($1,$2,$3,$4,$5,$6,nullif($7,0)) returning resolved_at`, resolution.ID, resolution.JobID, resolution.MessageID, resolution.Decision, resolution.Authority, resolution.Reason, resolution.ReservedWakeSequence).Scan(&resolution.ResolvedAt); err != nil {
		return spine.MessageResolution{}, false, err
	}
	if input.Decision == spine.ResolutionAbandon {
		if err := expectOne(tx.ExecContext(ctx, `update dorf.jobs set admission_open=false,workflow_attention=$2 where id=$1 and admission_open`, input.JobID, "Job abandoned by "+input.Authority+": "+input.Reason)); err != nil {
			return spine.MessageResolution{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return spine.MessageResolution{}, false, err
	}
	return resolution, true, nil
}

func resolutionMatches(resolution spine.MessageResolution, input MessageResolutionInput) bool {
	return resolution.JobID == input.JobID && resolution.MessageID == input.MessageID && resolution.Decision == input.Decision && resolution.Authority == input.Authority && resolution.Reason == input.Reason
}

func allocateWakeSequenceTx(ctx context.Context, tx *sql.Tx, jobID string) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `select greatest(
		coalesce((select max(sequence) from dorf.job_messages where job_id=$1),0),
		coalesce((select max(reserved_wake_sequence) from dorf.message_resolutions where job_id=$1),0)
	)+1`, jobID).Scan(&sequence)
	return sequence, err
}

func ensureInputsSettledTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	var sequence int64
	var state, reason string
	err := tx.QueryRowContext(ctx, `
		select m.sequence,coalesce(ar.state,''),coalesce(ar.attention,'')
		from dorf.job_messages m
		left join dorf.agent_runs ar on ar.message_id=m.id
		left join dorf.message_resolutions mr on mr.job_id=m.job_id and mr.message_id=m.id
		where m.job_id=$1 and not dorf.message_is_settled(ar.state,mr.decision)
		order by m.sequence limit 1`, jobID).Scan(&sequence, &state, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if reason != "" {
		state += ": " + reason
	}
	return fmt.Errorf("FIFO sequence %d is unresolved (%s); diagnose and resolve that exact input first", sequence, state)
}
