package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
	switch locked.WorkflowPhase {
	case "ready":
		if err := expectOneRows(queries.StartPublicationIntent(ctx, dbsql.StartPublicationIntentParams{JobID: jobID, Revision: revision})); err != nil {
			return spine.Job{}, spine.Action{}, spine.Action{}, err
		}
	case "publishing":
	case "published":
		proposal, proposalErr := queries.GetProposal(ctx, jobID)
		if proposalErr == nil && proposal.ProposedRevision == revision && proposal.ObservedRemoteHead == revision {
		} else {
			return spine.Job{}, spine.Action{}, spine.Action{}, fmt.Errorf("published Job is not stale at a later exact ready Revision")
		}
	case "publication-blocked":
		if err := expectOneRows(queries.ResumePublicationPhase(ctx, dbsql.ResumePublicationPhaseParams{JobID: jobID, Revision: revision})); err != nil {
			return spine.Job{}, spine.Action{}, spine.Action{}, err
		}
	default:
		return spine.Job{}, spine.Action{}, spine.Action{}, fmt.Errorf("exact Revision readiness is required before publication (phase %s)", locked.WorkflowPhase)
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
	return publicationAction(row.ID, row.JobID, row.MessageID, row.Kind, row.State, row.ExternalID, row.ExternalOutcome, row.ScopeKey), nil
}

func (s Store) PublicationActions(ctx context.Context, jobID, revision string) (spine.Action, spine.Action, error) {
	queries := dbsql.New(s.DB)
	load := func(kind spine.ActionKind) (spine.Action, error) {
		row, err := queries.GetPublicationAction(ctx, dbsql.GetPublicationActionParams{JobID: jobID, Kind: kind, ScopeKey: revision})
		if err != nil {
			return spine.Action{}, err
		}
		return publicationAction(row.ID, row.JobID, row.MessageID, row.Kind, row.State, row.ExternalID, row.ExternalOutcome, row.ScopeKey), nil
	}
	push, err := load(spine.ActionRepositoryPush)
	if err != nil {
		return spine.Action{}, spine.Action{}, err
	}
	pull, err := load(spine.ActionGitHubPullRequest)
	return push, pull, err
}

func (s Store) RecordPush(ctx context.Context, actionID, revision string) error {
	return expectOneRows(dbsql.New(s.DB).CompleteRepositoryPush(ctx, dbsql.CompleteRepositoryPushParams{Revision: revision, ActionID: actionID}))
}

func (s Store) RecordProposal(ctx context.Context, actionID string, proposal spine.GitHubProposal) error {
	if proposal.Number < 1 || proposal.URL == "" || proposal.BodyDigest == "" || proposal.ProposedRevision != proposal.ObservedRemoteHead {
		return fmt.Errorf("proposal receipt is incomplete or not exact-Revision fresh")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	locked, err := queries.GetProposalAuthorityJobForUpdate(ctx, proposal.JobID)
	if err != nil {
		return err
	}
	if locked.WorkflowPhase != "publishing" || locked.GithubRepository != proposal.Repository || locked.GithubInstallationID != proposal.InstallationID || locked.BaseBranch != proposal.BaseBranch || locked.Branch != proposal.HeadBranch || locked.Revision != proposal.ProposedRevision {
		return fmt.Errorf("proposal receipt conflicts with immutable Job authority or exact current Revision")
	}
	pushState, err := queries.GetRepositoryPushState(ctx, dbsql.GetRepositoryPushStateParams{JobID: proposal.JobID, Revision: locked.Revision})
	if err != nil || pushState != "succeeded" {
		return fmt.Errorf("proposal cannot be recorded before exact repository push success")
	}
	existingRow, err := queries.GetProposal(ctx, proposal.JobID)
	if err == nil {
		existing := githubProposal(existingRow)
		if existing.Number != proposal.Number || existing.Repository != proposal.Repository || existing.InstallationID != proposal.InstallationID || existing.BaseBranch != proposal.BaseBranch || existing.HeadBranch != proposal.HeadBranch {
			return fmt.Errorf("Job already owns conflicting GitHub proposal identity at pull request #%d", existing.Number)
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := expectOneRows(queries.UpsertProposal(ctx, dbsql.UpsertProposalParams{
		JobID: proposal.JobID, Repository: proposal.Repository, InstallationID: proposal.InstallationID,
		BaseBranch: proposal.BaseBranch, HeadBranch: proposal.HeadBranch, PRNumber: proposal.Number,
		PRURL: proposal.URL, ProposedRevision: proposal.ProposedRevision,
		ObservedRemoteHead: proposal.ObservedRemoteHead, BodyDigest: proposal.BodyDigest,
	})); err != nil {
		return err
	}
	if err := expectOneRows(queries.CompleteProposalAction(ctx, dbsql.CompleteProposalActionParams{ExternalID: strconvFormat(proposal.Number), BodyDigest: proposal.BodyDigest, ActionID: actionID, JobID: proposal.JobID, ProposedRevision: proposal.ProposedRevision})); err != nil {
		return err
	}
	if err := expectOneRows(queries.CompletePublication(ctx, dbsql.CompletePublicationParams{JobID: proposal.JobID, Revision: proposal.ProposedRevision})); err != nil {
		return err
	}
	return tx.Commit()
}

func strconvFormat(number int64) string { return fmt.Sprintf("%d", number) }

func (s Store) BlockPublication(ctx context.Context, jobID, revision, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "GitHub publication needs operator attention"
	}
	return expectOneRows(dbsql.New(s.DB).BlockPublicationPhase(ctx, dbsql.BlockPublicationPhaseParams{Reason: reason, JobID: jobID, Revision: revision}))
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
	current, err := queries.GetProposalCurrentRevision(ctx, jobID)
	if err != nil {
		return nil, err
	}
	proposal.Stale = proposal.ProposedRevision != current || proposal.ObservedRemoteHead != current
	return &proposal, nil
}

func publicationAction(id, jobID, messageID string, kind spine.ActionKind, state spine.ActionState, externalID, outcome, scope string) spine.Action {
	return spine.Action{
		ID: id, JobID: jobID, MessageID: messageID, Kind: kind, State: state,
		ExternalID: externalID, Outcome: outcome, Scope: scope,
	}
}

func githubProposal(row dbsql.GetProposalRow) spine.GitHubProposal {
	return spine.GitHubProposal{
		JobID: row.JobID, Repository: row.Repository, InstallationID: row.InstallationID,
		BaseBranch: row.BaseBranch, HeadBranch: row.HeadBranch, Number: row.PRNumber, URL: row.PRURL,
		ProposedRevision: row.ProposedRevision, ObservedRemoteHead: row.ObservedRemoteHead, BodyDigest: row.BodyDigest,
	}
}
