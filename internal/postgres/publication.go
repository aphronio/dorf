package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/aphronio/dorf/internal/spine"
)

func (s Store) BeginPublication(ctx context.Context, jobID, revision string) (spine.Job, spine.Action, spine.Action, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetPublicationJobForUpdate(ctx, jobID)
	if err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, err
	}
	if !locked.AdmissionOpen || locked.CleanupState != spine.CleanupPending {
		return spine.Job{}, spine.Action{}, spine.Action{}, fmt.Errorf("publication cannot start after Job admission closes or cleanup begins")
	}
	if locked.Revision != revision || !ValidRevision(revision) {
		return spine.Job{}, spine.Action{}, spine.Action{}, fmt.Errorf("publication Revision %s conflicts with exact ready Revision %s", revision, locked.Revision)
	}
	if err := githubapi.ValidateAuthority(locked.Repository, locked.GithubRepository, locked.GithubInstallationID, locked.BaseBranch, locked.Branch); err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, fmt.Errorf("publication authority unresolved: %w", err)
	}
	intentStarted := false
	for _, kind := range []spine.ActionKind{spine.ActionRepositoryPush, spine.ActionGitHubPullRequest} {
		_, actionErr := queries.GetPublicationAction(ctx, dbsql.GetPublicationActionParams{JobID: jobID, Kind: kind, ScopeKey: revision})
		if actionErr == nil {
			intentStarted = true
			continue
		}
		if !errors.Is(actionErr, sql.ErrNoRows) {
			return spine.Job{}, spine.Action{}, spine.Action{}, actionErr
		}
	}
	if !intentStarted {
		unsettled, err := queries.CountUnsettledInputs(ctx, jobID)
		if err != nil {
			return spine.Job{}, spine.Action{}, spine.Action{}, err
		}
		latest, err := queries.GetLatestFollowRun(ctx, jobID)
		if err != nil || unsettled != 0 || latest.State != spine.AgentRunCompleted || latest.Role != "implement" || !latest.Observed {
			return spine.Job{}, spine.Action{}, spine.Action{}, fmt.Errorf("publication cannot begin before the latest implementation input is finished and observed")
		}
		observed, err := queries.GetEvidenceIdentity(ctx, spine.EvidenceID(latest.ID, "git-revision"))
		if err != nil || observed.JobID != jobID || observed.Kind != "git-revision" || observed.AgentRunID != latest.ID || observed.Revision != revision {
			return spine.Job{}, spine.Action{}, spine.Action{}, fmt.Errorf("publication cannot begin before the latest implementation input is finished and observed")
		}
	}
	push, err := beginPublicationAction(ctx, queries, jobID, spine.ActionRepositoryPush, revision)
	if err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, err
	}
	pull, err := beginPublicationAction(ctx, queries, jobID, spine.ActionGitHubPullRequest, revision)
	if err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, err
	}
	if err := tx.Commit(); err != nil {
		return spine.Job{}, spine.Action{}, spine.Action{}, err
	}
	job, err := s.Job(ctx, jobID)
	return job, push, pull, err
}

func beginPublicationAction(ctx context.Context, queries *dbsql.Queries, jobID string, kind spine.ActionKind, revision string) (spine.Action, error) {
	id := spine.ScopedActionID(jobID, kind, revision)
	if err := queries.InsertPublicationAction(ctx, dbsql.InsertPublicationActionParams{ID: id, JobID: jobID, Kind: kind, ScopeKey: revision}); err != nil {
		return spine.Action{}, err
	}
	row, err := queries.GetPublicationActionForUpdate(ctx, dbsql.GetPublicationActionForUpdateParams{ID: id, JobID: jobID, Kind: kind, ScopeKey: revision})
	if err != nil {
		return spine.Action{}, err
	}
	return publicationAction(row.ID, row.JobID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt), nil
}

func (s Store) PublicationActions(ctx context.Context, jobID, revision string) (spine.Action, spine.Action, error) {
	queries := dbsql.New(s.DB)
	load := func(kind spine.ActionKind) (spine.Action, error) {
		row, err := queries.GetPublicationAction(ctx, dbsql.GetPublicationActionParams{JobID: jobID, Kind: kind, ScopeKey: revision})
		if err != nil {
			return spine.Action{}, err
		}
		return publicationAction(row.ID, row.JobID, row.Kind, row.State, row.ScopeKey, row.CreatedAt, row.SettledAt), nil
	}
	push, err := load(spine.ActionRepositoryPush)
	if err != nil {
		return spine.Action{}, spine.Action{}, err
	}
	pull, err := load(spine.ActionGitHubPullRequest)
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

func (s Store) RecordProposal(ctx context.Context, actionID string, proposal spine.GitHubProposal) error {
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
	if locked.Revision != proposal.ProposedRevision || !locked.AdmissionOpen || locked.CleanupState != spine.CleanupPending {
		return fmt.Errorf("Proposal conflicts with the exact current Job Revision")
	}
	pushState, err := queries.GetRepositoryPushState(ctx, dbsql.GetRepositoryPushStateParams{JobID: proposal.JobID, Revision: locked.Revision})
	if err != nil || pushState != "succeeded" {
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

func (s Store) Proposal(ctx context.Context, jobID string) (*spine.GitHubProposal, error) {
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

func publicationAction(id, jobID string, kind spine.ActionKind, state spine.ActionState, scope string, createdAt time.Time, settledAt sql.NullTime) spine.Action {
	action := spine.Action{ID: id, JobID: jobID, Kind: kind, State: state, Scope: scope, CreatedAt: createdAt}
	if settledAt.Valid {
		action.SettledAt = settledAt.Time
	}
	return action
}

func githubProposal(row dbsql.DorfGithubProposal) spine.GitHubProposal {
	return spine.GitHubProposal{
		JobID: row.JobID, Number: row.PRNumber, URL: row.PRURL,
		ProposedRevision: row.ProposedRevision, BodyDigest: row.BodyDigest,
	}
}
