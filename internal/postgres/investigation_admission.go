package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) AdmitInvestigation(ctx context.Context, input investigation.Admission, queueName string) (core.Job, bool, error) {
	normalized, err := investigation.NormalizeAdmission(input)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", investigation.ErrInvalidAdmission, err)
	}
	normalized.JobAdmission, err = normalizeCoreAdmission(normalized.JobAdmission)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", investigation.ErrInvalidAdmission, err)
	}
	job, created, err := admitJob(ctx, s, normalized.JobAdmission, queueName, investigation.TaskName, investigation.TaskKey(core.JobID(normalized.AdmissionKey)), func(ctx context.Context, queries *dbsql.Queries, ids admittedJobIDs) error {
		if _, err := queries.InsertCodebaseInvestigationSource(ctx, investigationSourceParams(ids.jobID, normalized.Source)); err != nil {
			return err
		}
		stored, err := queries.GetCodebaseInvestigationSource(ctx, ids.jobID)
		if err != nil {
			return err
		}
		if stored.JobID != ids.jobID || investigationSourceFromValues("", stored.Repository, stored.Revision) != normalized.Source {
			return fmt.Errorf("%w: %q", ErrAdmissionConflict, normalized.AdmissionKey)
		}
		if _, err := queries.InsertAdmittedAgentRun(ctx, dbsql.InsertAdmittedAgentRunParams{
			ID: core.AgentRunID(ids.messageID), JobID: ids.jobID, MessageID: ids.messageID, Role: investigation.InitialAgentRole,
			InputRevision: nullableString(normalized.Source.Revision), Capability: nullableString(investigation.InitialAgentCapability), SandboxID: ids.sandboxID,
		}); err != nil {
			return err
		}
		return nil
	})
	if errors.Is(err, ErrAdmissionConflict) {
		err = fmt.Errorf("%w: %w", investigation.ErrAdmissionConflict, err)
	}
	return job, created, err
}
