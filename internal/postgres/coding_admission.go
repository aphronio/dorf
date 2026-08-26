package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) AdmitCoding(ctx context.Context, input coding.Admission) (core.Job, bool, error) {
	normalized, err := coding.NormalizeAdmission(input)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", coding.ErrInvalidAdmission, err)
	}
	normalized.JobAdmission, err = normalizeCoreAdmission(normalized.JobAdmission)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", coding.ErrInvalidAdmission, err)
	}
	job, created, err := admitJob(ctx, s, normalized.JobAdmission, func(ctx context.Context, queries *dbsql.Queries, ids admittedJobIDs) error {
		if _, err := queries.InsertCodingToProposalInput(ctx, dbsql.InsertCodingToProposalInputParams{
			JobID: ids.jobID, Repository: normalized.Repository, StartingRevision: normalized.Revision, Revision: normalized.Revision,
			Branch: normalized.Branch, GithubRepository: normalized.GitHubRepository,
			GithubInstallationID: normalized.GitHubInstallation, BaseBranch: normalized.BaseBranch,
		}); err != nil {
			return err
		}
		stored, err := queries.GetCodingToProposalInput(ctx, ids.jobID)
		if err != nil {
			return err
		}
		if stored.JobID != ids.jobID || stored.Repository != normalized.Repository || stored.StartingRevision != normalized.Revision ||
			stored.Branch != normalized.Branch || stored.GithubRepository != normalized.GitHubRepository ||
			stored.GithubInstallationID != normalized.GitHubInstallation || stored.BaseBranch != normalized.BaseBranch {
			return fmt.Errorf("%w: %q", ErrAdmissionConflict, normalized.AdmissionKey)
		}
		if _, err := queries.InsertAdmittedAgentRun(ctx, dbsql.InsertAdmittedAgentRunParams{
			ID: core.AgentRunID(ids.messageID), JobID: ids.jobID, MessageID: ids.messageID, Role: coding.InitialAgentRole,
			InputRevision: nullableString(normalized.Revision), SandboxID: ids.sandboxID,
		}); err != nil {
			return err
		}
		return queries.InsertInitialRevision(ctx, dbsql.InsertInitialRevisionParams{JobID: ids.jobID, OID: normalized.Revision, Branch: normalized.Branch})
	})
	if errors.Is(err, ErrAdmissionConflict) {
		err = fmt.Errorf("%w: %w", coding.ErrAdmissionConflict, err)
	}
	return job, created, err
}
