package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/aphronio/dorf/internal/spine"
)

func (s Store) Outcome(ctx context.Context, jobID string) (*spine.JobOutcome, error) {
	row, err := dbsql.New(s.DB).GetOutcome(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	outcome := jobOutcome(row.JobID, row.Outcome, row.ObservedState, row.ObservedMerged, row.MergeCommitOID, row.ObservedAt)
	return &outcome, nil
}

func (s Store) RecordOutcome(ctx context.Context, receipt spine.JobOutcome) (spine.JobOutcome, bool, error) {
	if err := validateOutcomeReceipt(receipt); err != nil {
		return spine.JobOutcome{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.JobOutcome{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetOutcomeJobForUpdate(ctx, receipt.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spine.JobOutcome{}, false, ErrNotFound
		}
		return spine.JobOutcome{}, false, err
	}
	existingRow, err := queries.GetOutcome(ctx, receipt.JobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return spine.JobOutcome{}, false, err
	}
	if err == nil {
		existing := jobOutcome(existingRow.JobID, existingRow.Outcome, existingRow.ObservedState, existingRow.ObservedMerged, existingRow.MergeCommitOID, existingRow.ObservedAt)
		if existing.Kind != receipt.Kind {
			return spine.JobOutcome{}, false, fmt.Errorf("Job %s already has immutable %s outcome; refusing conflicting %s outcome", receipt.JobID, existing.Kind, receipt.Kind)
		}
		return existing, false, nil
	}
	if locked.WorkflowPhase != "published" {
		return spine.JobOutcome{}, false, fmt.Errorf("Job outcome requires the exact current published Revision")
	}
	proposalRow, err := queries.GetProposal(ctx, receipt.JobID)
	if err != nil {
		return spine.JobOutcome{}, false, fmt.Errorf("load exact GitHub proposal for outcome: %w", err)
	}
	proposal := githubProposal(proposalRow)
	if proposal.ProposedRevision != locked.Revision {
		return spine.JobOutcome{}, false, fmt.Errorf("Job outcome requires the exact current published Proposal")
	}
	inserted, err := queries.InsertOutcome(ctx, dbsql.InsertOutcomeParams{
		JobID: receipt.JobID, Outcome: receipt.Kind,
		ObservedState: receipt.ObservedState, ObservedMerged: receipt.ObservedMerged,
		MergeCommitOID: receipt.MergeCommitOID, ObservedAt: receipt.ObservedAt,
	})
	if err != nil {
		return spine.JobOutcome{}, false, err
	}
	if err := expectOneRows(queries.ClearOutcomeAttention(ctx, dbsql.ClearOutcomeAttentionParams{JobID: receipt.JobID, Revision: locked.Revision})); err != nil {
		return spine.JobOutcome{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.JobOutcome{}, false, err
	}
	stored := jobOutcome(inserted.JobID, inserted.Outcome, inserted.ObservedState, inserted.ObservedMerged, inserted.MergeCommitOID, inserted.ObservedAt)
	return stored, true, nil
}

func jobOutcome(jobID string, kind spine.JobOutcomeKind, observedState string, observedMerged bool, mergeCommitOID string, observedAt time.Time) spine.JobOutcome {
	return spine.JobOutcome{
		JobID: jobID, Kind: kind, ObservedState: observedState,
		ObservedMerged: observedMerged, MergeCommitOID: mergeCommitOID, ObservedAt: observedAt,
	}
}

func validateOutcomeReceipt(receipt spine.JobOutcome) error {
	if strings.TrimSpace(receipt.JobID) == "" || receipt.ObservedAt.IsZero() {
		return fmt.Errorf("Job outcome receipt is incomplete")
	}
	switch receipt.Kind {
	case spine.OutcomeAccepted:
		if receipt.ObservedState != "closed" || !receipt.ObservedMerged || !ValidRevision(receipt.MergeCommitOID) {
			return fmt.Errorf("accepted outcome requires an exact merged GitHub pull request and merge commit OID")
		}
	case spine.OutcomeRejected:
		if receipt.ObservedState != "closed" || receipt.ObservedMerged || receipt.MergeCommitOID != "" {
			return fmt.Errorf("rejected outcome requires an exact closed, unmerged GitHub pull request")
		}
	case spine.OutcomeAbandoned:
		if receipt.ObservedMerged || receipt.MergeCommitOID != "" || (receipt.ObservedState != "open" && receipt.ObservedState != "closed") {
			return fmt.Errorf("abandoned outcome refuses an already merged GitHub pull request")
		}
	default:
		return fmt.Errorf("outcome must be accepted, rejected, or abandoned")
	}
	return nil
}
