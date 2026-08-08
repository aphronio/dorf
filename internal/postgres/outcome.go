package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/spine"
)

func (s Store) Outcome(ctx context.Context, jobID string) (*spine.JobOutcome, error) {
	return scanOutcome(s.DB.QueryRowContext(ctx, outcomeQuery, jobID))
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
	var currentRevision, phase string
	if err := tx.QueryRowContext(ctx, `select revision,workflow_phase from dorf.jobs where id=$1 for update`, receipt.JobID).Scan(&currentRevision, &phase); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spine.JobOutcome{}, false, ErrNotFound
		}
		return spine.JobOutcome{}, false, err
	}
	existing, err := scanOutcome(tx.QueryRowContext(ctx, outcomeQuery, receipt.JobID))
	if err != nil {
		return spine.JobOutcome{}, false, err
	}
	if existing != nil {
		if existing.Kind != receipt.Kind {
			return spine.JobOutcome{}, false, fmt.Errorf("Job %s already has immutable %s outcome; refusing conflicting %s outcome", receipt.JobID, existing.Kind, receipt.Kind)
		}
		return *existing, false, nil
	}
	if phase != "published" || receipt.ProposedRevision != currentRevision {
		return spine.JobOutcome{}, false, fmt.Errorf("Job outcome requires the exact current published Revision")
	}
	var proposal spine.GitHubProposal
	if err := tx.QueryRowContext(ctx, `select job_id,repository,installation_id,base_branch,head_branch,pr_number,pr_url,proposed_revision,observed_remote_head,body_digest from dorf.github_proposals where job_id=$1`, receipt.JobID).Scan(
		&proposal.JobID, &proposal.Repository, &proposal.InstallationID, &proposal.BaseBranch, &proposal.HeadBranch, &proposal.Number, &proposal.URL, &proposal.ProposedRevision, &proposal.ObservedRemoteHead, &proposal.BodyDigest,
	); err != nil {
		return spine.JobOutcome{}, false, fmt.Errorf("load exact GitHub proposal for outcome: %w", err)
	}
	if receipt.Repository != proposal.Repository || receipt.InstallationID != proposal.InstallationID || receipt.BaseBranch != proposal.BaseBranch || receipt.HeadBranch != proposal.HeadBranch || receipt.Number != proposal.Number || receipt.URL != proposal.URL || receipt.ProposedRevision != proposal.ProposedRevision || receipt.ObservedHead != proposal.ProposedRevision || proposal.ObservedRemoteHead != proposal.ProposedRevision {
		return spine.JobOutcome{}, false, fmt.Errorf("observed GitHub pull request conflicts with the exact stored proposal identity or proposed Revision")
	}
	_, err = tx.ExecContext(ctx, `
		insert into dorf.job_outcomes(
			job_id,outcome,repository,installation_id,base_branch,head_branch,pr_number,pr_url,
			proposed_revision,observed_head,observed_state,observed_merged,merge_commit_oid,observed_at
		) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,nullif($13,''),$14)`,
		receipt.JobID, receipt.Kind, receipt.Repository, receipt.InstallationID, receipt.BaseBranch,
		receipt.HeadBranch, receipt.Number, receipt.URL, receipt.ProposedRevision, receipt.ObservedHead,
		receipt.ObservedState, receipt.ObservedMerged, receipt.MergeCommitOID, receipt.ObservedAt,
	)
	if err != nil {
		return spine.JobOutcome{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `update dorf.jobs set workflow_attention=null where id=$1`, receipt.JobID); err != nil {
		return spine.JobOutcome{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.JobOutcome{}, false, err
	}
	stored, err := s.Outcome(ctx, receipt.JobID)
	if err != nil || stored == nil {
		return spine.JobOutcome{}, false, err
	}
	return *stored, true, nil
}

const outcomeQuery = `select job_id,outcome,repository,installation_id,base_branch,head_branch,
	pr_number,pr_url,proposed_revision,observed_head,observed_state,observed_merged,
	coalesce(merge_commit_oid,''),observed_at from dorf.job_outcomes where job_id=$1`

func scanOutcome(row rowScanner) (*spine.JobOutcome, error) {
	var outcome spine.JobOutcome
	err := row.Scan(&outcome.JobID, &outcome.Kind, &outcome.Repository, &outcome.InstallationID,
		&outcome.BaseBranch, &outcome.HeadBranch, &outcome.Number, &outcome.URL,
		&outcome.ProposedRevision, &outcome.ObservedHead, &outcome.ObservedState,
		&outcome.ObservedMerged, &outcome.MergeCommitOID, &outcome.ObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &outcome, err
}

func validateOutcomeReceipt(receipt spine.JobOutcome) error {
	if strings.TrimSpace(receipt.JobID) == "" || receipt.Number < 1 || strings.TrimSpace(receipt.URL) == "" || !ValidRevision(receipt.ProposedRevision) || !ValidRevision(receipt.ObservedHead) || receipt.ObservedAt.IsZero() {
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
