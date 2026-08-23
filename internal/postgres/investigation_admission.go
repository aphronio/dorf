package postgres

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) AdmitInvestigation(ctx context.Context, input investigation.Admission) (core.Job, bool, error) {
	normalized, err := investigation.NormalizeAdmission(input)
	if err != nil {
		return core.Job{}, false, err
	}
	normalized.JobAdmission, err = normalizeCoreAdmission(normalized.JobAdmission)
	if err != nil {
		return core.Job{}, false, err
	}
	return admitJob(ctx, s, normalized.JobAdmission, func(ctx context.Context, queries *dbsql.Queries, ids admittedJobIDs) error {
		if _, err := queries.InsertCodebaseInvestigationSource(ctx, investigationSourceParams(ids.jobID, normalized.Source)); err != nil {
			return err
		}
		stored, err := queries.GetCodebaseInvestigationSource(ctx, ids.jobID)
		if err != nil {
			return err
		}
		if stored.JobID != ids.jobID || investigationSourceFromValues("", stored.Kind, stored.Repository, stored.Revision, stored.BundleDigest, stored.BundleByteSize) != normalized.Source {
			return fmt.Errorf("admission key %q is already bound to different complete Job input", normalized.AdmissionKey)
		}
		if _, err := queries.InsertAdmittedAgentRun(ctx, dbsql.InsertAdmittedAgentRunParams{
			ID: core.AgentRunID(ids.messageID), JobID: ids.jobID, MessageID: ids.messageID, Role: investigation.InitialAgentRole,
			InputRevision: nullableString(normalized.Source.Revision), Capability: nullableString(investigation.InitialAgentCapability), SandboxID: ids.sandboxID,
		}); err != nil {
			return err
		}
		return nil
	})
}
