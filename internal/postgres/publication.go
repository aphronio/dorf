package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) BeginPublication(ctx context.Context, jobID, revision string) (coding.Job, core.Action, core.Action, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return coding.Job{}, core.Action{}, core.Action{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetPublicationJobForUpdate(ctx, jobID)
	if err != nil {
		return coding.Job{}, core.Action{}, core.Action{}, err
	}
	if !locked.AdmissionOpen || locked.CleanupState != core.CleanupPending {
		return coding.Job{}, core.Action{}, core.Action{}, fmt.Errorf("publication cannot start after Job admission closes or cleanup begins")
	}
	if locked.Revision != revision || !ValidRevision(revision) {
		return coding.Job{}, core.Action{}, core.Action{}, fmt.Errorf("publication Revision %s conflicts with exact ready Revision %s", revision, locked.Revision)
	}
	if err := githubapi.ValidateAuthority(locked.Repository, locked.GithubRepository, locked.GithubInstallationID, locked.BaseBranch, locked.Branch); err != nil {
		return coding.Job{}, core.Action{}, core.Action{}, fmt.Errorf("publication authority unresolved: %w", err)
	}
	intentStarted := false
	for _, kind := range []core.ActionKind{coding.ActionRepositoryPush, coding.ActionGitHubPullRequest} {
		row, actionErr := queries.GetScopedAction(ctx, dbsql.GetScopedActionParams{JobID: jobID, Kind: kind, ScopeKey: revision})
		if actionErr == nil {
			if _, exactErr := exactScopedAction(row, jobID, kind, revision); exactErr != nil {
				return coding.Job{}, core.Action{}, core.Action{}, exactErr
			}
			intentStarted = true
			continue
		}
		if !errors.Is(actionErr, sql.ErrNoRows) {
			return coding.Job{}, core.Action{}, core.Action{}, actionErr
		}
	}
	if !intentStarted {
		unsettled, err := queries.CountUnsettledInputs(ctx, jobID)
		if err != nil {
			return coding.Job{}, core.Action{}, core.Action{}, err
		}
		latestInput, err := queries.GetLatestImplementationRun(ctx, jobID)
		if err != nil || latestInput.State != core.AgentRunCompleted {
			return coding.Job{}, core.Action{}, core.Action{}, fmt.Errorf("publication cannot begin before the latest implementation input is finished and observed")
		}
		latest, err := queries.GetLatestTurnStartRun(ctx, jobID)
		if err != nil || unsettled != 0 || latest.State != core.AgentRunCompleted || latest.Role != "implement" || !latest.Observed {
			return coding.Job{}, core.Action{}, core.Action{}, fmt.Errorf("publication cannot begin before the latest implementation input is finished and observed")
		}
		observed, err := queries.GetEvidenceIdentity(ctx, core.EvidenceID(latest.ID, "git-revision"))
		if err != nil || observed.JobID != jobID || observed.Kind != "git-revision" || observed.AgentRunID != latest.ID || observed.Revision != revision {
			return coding.Job{}, core.Action{}, core.Action{}, fmt.Errorf("publication cannot begin before the latest implementation input is finished and observed")
		}
	}
	push, err := beginPublicationAction(ctx, queries, jobID, coding.ActionRepositoryPush, revision)
	if err != nil {
		return coding.Job{}, core.Action{}, core.Action{}, err
	}
	pull, err := beginPublicationAction(ctx, queries, jobID, coding.ActionGitHubPullRequest, revision)
	if err != nil {
		return coding.Job{}, core.Action{}, core.Action{}, err
	}
	if err := tx.Commit(); err != nil {
		return coding.Job{}, core.Action{}, core.Action{}, err
	}
	job, err := s.CodingJob(ctx, jobID)
	return job, push, pull, err
}

func beginPublicationAction(ctx context.Context, queries *dbsql.Queries, jobID string, kind core.ActionKind, revision string) (core.Action, error) {
	id := core.ScopedActionID(jobID, kind, revision)
	if _, err := queries.InsertScopedAction(ctx, dbsql.InsertScopedActionParams{ID: id, JobID: jobID, Kind: kind, ScopeKey: revision}); err != nil {
		return core.Action{}, err
	}
	row, err := queries.GetActionForUpdate(ctx, dbsql.GetActionForUpdateParams{ID: id, JobID: jobID, Kind: kind})
	if err != nil {
		return core.Action{}, err
	}
	return exactScopedAction(row, jobID, kind, revision)
}

func (s Store) PublicationActions(ctx context.Context, jobID, revision string) (core.Action, core.Action, error) {
	queries := dbsql.New(s.DB)
	load := func(kind core.ActionKind) (core.Action, error) {
		row, err := queries.GetScopedAction(ctx, dbsql.GetScopedActionParams{JobID: jobID, Kind: kind, ScopeKey: revision})
		if err != nil {
			return core.Action{}, err
		}
		return exactScopedAction(row, jobID, kind, revision)
	}
	push, err := load(coding.ActionRepositoryPush)
	if err != nil {
		return core.Action{}, core.Action{}, err
	}
	pull, err := load(coding.ActionGitHubPullRequest)
	return push, pull, err
}

func (s Store) RecordPush(ctx context.Context, actionID, revision string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	if err := expectOneRows(queries.CompleteRepositoryPush(ctx, dbsql.CompleteRepositoryPushParams{Revision: revision, ActionID: actionID})); err != nil {
		return err
	}
	if err := queries.ClearPublicationAttentionForAction(ctx, dbsql.ClearPublicationAttentionForActionParams{Revision: revision, ActionID: sql.NullString{String: actionID, Valid: true}}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) RecordProposal(ctx context.Context, actionID string, proposal coding.Proposal) error {
	if proposal.Number < 1 || proposal.URL == "" || proposal.BodyDigest == "" || !ValidRevision(proposal.ProposedRevision) {
		return fmt.Errorf("Proposal is incomplete or not exact-Revision")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetProposalJobForUpdate(ctx, proposal.JobID)
	if err != nil {
		return err
	}
	if locked.Revision != proposal.ProposedRevision || !locked.AdmissionOpen || locked.CleanupState != core.CleanupPending {
		return fmt.Errorf("Proposal conflicts with the exact current Job Revision")
	}
	pushRow, err := queries.GetScopedAction(ctx, dbsql.GetScopedActionParams{JobID: proposal.JobID, Kind: coding.ActionRepositoryPush, ScopeKey: locked.Revision})
	if err != nil {
		return fmt.Errorf("proposal cannot be recorded before exact repository push success")
	}
	push, err := exactScopedAction(pushRow, proposal.JobID, coding.ActionRepositoryPush, locked.Revision)
	if err != nil || push.State != core.ActionSucceeded {
		return fmt.Errorf("proposal cannot be recorded before exact repository push success")
	}
	existingRow, err := queries.GetProposal(ctx, proposal.JobID)
	if err == nil {
		existing := githubProposal(existingRow)
		if existing.Number != proposal.Number {
			return fmt.Errorf("Job already owns conflicting GitHub proposal identity at pull request #%d", existing.Number)
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := expectOneRows(queries.UpsertProposal(ctx, dbsql.UpsertProposalParams{
		JobID: proposal.JobID, PRNumber: proposal.Number, PRURL: proposal.URL,
		ProposedRevision: proposal.ProposedRevision, BodyDigest: proposal.BodyDigest,
	})); err != nil {
		return err
	}
	if err := expectOneRows(queries.CompleteProposalAction(ctx, dbsql.CompleteProposalActionParams{ActionID: actionID, JobID: proposal.JobID, ProposedRevision: proposal.ProposedRevision})); err != nil {
		return err
	}
	if err := queries.ClearPublicationAttention(ctx, dbsql.ClearPublicationAttentionParams{JobID: proposal.JobID, Revision: proposal.ProposedRevision, ActionID: sql.NullString{String: actionID, Valid: true}}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s Store) BlockPublication(ctx context.Context, jobID, revision, actionID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "GitHub publication needs operator attention"
	}
	return expectOneRows(dbsql.New(s.DB).SetPublicationAttention(ctx, dbsql.SetPublicationAttentionParams{Reason: reason, ActionID: actionID, JobID: jobID, Revision: revision}))
}

func (s Store) Proposal(ctx context.Context, jobID string) (*coding.Proposal, error) {
	queries := dbsql.New(s.DB)
	row, err := queries.GetProposal(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	proposal := githubProposal(row)
	return &proposal, nil
}

func githubProposal(row dbsql.DorfGithubProposal) coding.Proposal {
	return coding.Proposal{
		JobID: row.JobID, Number: row.PRNumber, URL: row.PRURL,
		ProposedRevision: row.ProposedRevision, BodyDigest: row.BodyDigest,
	}
}
