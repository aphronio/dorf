package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) Outcome(ctx context.Context, jobID string) (*coding.Outcome, error) {
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

func (s Store) RecordOutcome(ctx context.Context, receipt coding.Outcome) (coding.Outcome, bool, error) {
	if err := validateOutcomeReceipt(receipt); err != nil {
		return coding.Outcome{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return coding.Outcome{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetOutcomeJobForUpdate(ctx, receipt.JobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return coding.Outcome{}, false, ErrNotFound
		}
		return coding.Outcome{}, false, err
	}
	existingRow, err := queries.GetOutcome(ctx, receipt.JobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return coding.Outcome{}, false, err
	}
	if err == nil {
		existing := jobOutcome(existingRow.JobID, existingRow.Outcome, existingRow.ObservedState, existingRow.ObservedMerged, existingRow.MergeCommitOID, existingRow.ObservedAt)
		if existing.Kind != receipt.Kind || existing.ObservedState != receipt.ObservedState || existing.ObservedMerged != receipt.ObservedMerged || existing.MergeCommitOID != receipt.MergeCommitOID {
			return coding.Outcome{}, false, fmt.Errorf("Job %s already has immutable %s outcome authority; refusing contradictory retry", receipt.JobID, existing.Kind)
		}
		return existing, false, nil
	}
	if !locked.AdmissionOpen || locked.CleanupState != core.CleanupPending {
		return coding.Outcome{}, false, fmt.Errorf("Job outcome cannot be recorded after admission closes or cleanup begins")
	}
	proposalRow, proposalErr := queries.GetProposal(ctx, receipt.JobID)
	switch {
	case errors.Is(proposalErr, sql.ErrNoRows):
		if receipt.Kind != coding.OutcomeAbandoned || receipt.ObservedState != "" {
			return coding.Outcome{}, false, fmt.Errorf("accepted and rejected outcomes require the exact current published Proposal")
		}
		intent, err := queries.OutcomePublicationIntentExists(ctx, receipt.JobID)
		if err != nil {
			return coding.Outcome{}, false, err
		}
		if intent {
			return coding.Outcome{}, false, fmt.Errorf("Job abandonment must wait for the started pull-request Action to reconcile its exact Proposal")
		}
	case proposalErr != nil:
		return coding.Outcome{}, false, fmt.Errorf("load exact GitHub proposal for outcome: %w", proposalErr)
	default:
		proposal := githubProposal(proposalRow)
		if proposal.ProposedRevision != locked.Revision || receipt.ObservedState == "" {
			return coding.Outcome{}, false, fmt.Errorf("Job outcome requires the exact current published Proposal")
		}
		if receipt.Kind != coding.OutcomeAbandoned {
			settled, err := queries.OutcomeImplementationSettled(ctx, receipt.JobID)
			if err != nil {
				return coding.Outcome{}, false, err
			}
			if !settled {
				return coding.Outcome{}, false, fmt.Errorf("Job outcome requires all admitted implementation input to be finished and observed")
			}
		}
	}
	inserted, err := queries.InsertOutcome(ctx, dbsql.InsertOutcomeParams{
		JobID: receipt.JobID, Outcome: receipt.Kind,
		ObservedState: receipt.ObservedState, ObservedMerged: receipt.ObservedMerged,
		MergeCommitOID: receipt.MergeCommitOID, ObservedAt: receipt.ObservedAt,
	})
	if err != nil {
		return coding.Outcome{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return coding.Outcome{}, false, err
	}
	stored := jobOutcome(inserted.JobID, inserted.Outcome, inserted.ObservedState, inserted.ObservedMerged, inserted.MergeCommitOID, inserted.ObservedAt)
	return stored, true, nil
}

func jobOutcome(jobID string, kind coding.OutcomeKind, observedState string, observedMerged bool, mergeCommitOID string, observedAt time.Time) coding.Outcome {
	return coding.Outcome{
		JobID: jobID, Kind: kind, ObservedState: observedState,
		ObservedMerged: observedMerged, MergeCommitOID: mergeCommitOID, ObservedAt: observedAt,
	}
}

func validateOutcomeReceipt(receipt coding.Outcome) error {
	if strings.TrimSpace(receipt.JobID) == "" || receipt.ObservedAt.IsZero() {
		return fmt.Errorf("Job outcome receipt is incomplete")
	}
	switch receipt.Kind {
	case coding.OutcomeAccepted:
		if receipt.ObservedState != "closed" || !receipt.ObservedMerged || !ValidRevision(receipt.MergeCommitOID) {
			return fmt.Errorf("accepted outcome requires an exact merged GitHub pull request and merge commit OID")
		}
	case coding.OutcomeRejected:
		if receipt.ObservedState != "closed" || receipt.ObservedMerged || receipt.MergeCommitOID != "" {
			return fmt.Errorf("rejected outcome requires an exact closed, unmerged GitHub pull request")
		}
	case coding.OutcomeAbandoned:
		if receipt.ObservedMerged || receipt.MergeCommitOID != "" || (receipt.ObservedState != "" && receipt.ObservedState != "open" && receipt.ObservedState != "closed") {
			return fmt.Errorf("abandoned outcome refuses an already merged GitHub pull request")
		}
	default:
		return fmt.Errorf("outcome must be accepted, rejected, or abandoned")
	}
	return nil
}
