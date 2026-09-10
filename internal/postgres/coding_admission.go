package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) AdmitCoding(ctx context.Context, input coding.Admission, queueName string) (core.Job, bool, error) {
	normalized, err := coding.NormalizeAdmission(input)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", coding.ErrInvalidAdmission, err)
	}
	normalized.JobAdmission, err = normalizeCoreAdmission(normalized.JobAdmission)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", coding.ErrInvalidAdmission, err)
	}
	job, created, err := admitJob(ctx, s, normalized.JobAdmission, queueName, coding.TaskName, coding.TaskKey(core.JobID(normalized.AdmissionKey)), func(ctx context.Context, queries *dbsql.Queries, jobID string) error {
		if _, err := queries.InsertCodingToProposalInput(ctx, dbsql.InsertCodingToProposalInputParams{
			JobID: jobID, Repository: normalized.Repository, StartingRevision: normalized.Revision, Revision: normalized.Revision,
			Branch: normalized.Branch, GithubRepository: normalized.GitHubRepository,
			GithubInstallationID: normalized.GitHubInstallation, BaseBranch: normalized.BaseBranch,
		}); err != nil {
			return err
		}
		stored, err := queries.GetCodingToProposalInput(ctx, jobID)
		if err != nil {
			return err
		}
		if stored.JobID != jobID || stored.Repository != normalized.Repository || stored.StartingRevision != normalized.Revision ||
			stored.Branch != normalized.Branch || stored.GithubRepository != normalized.GitHubRepository ||
			stored.GithubInstallationID != normalized.GitHubInstallation || stored.BaseBranch != normalized.BaseBranch {
			return fmt.Errorf("%w: %q", ErrAdmissionConflict, normalized.AdmissionKey)
		}

		return queries.InsertInitialRevision(ctx, dbsql.InsertInitialRevisionParams{JobID: jobID, OID: normalized.Revision, Branch: normalized.Branch})
	})
	if errors.Is(err, ErrAdmissionConflict) {
		err = fmt.Errorf("%w: %w", coding.ErrAdmissionConflict, err)
	}
	return job, created, err
}
