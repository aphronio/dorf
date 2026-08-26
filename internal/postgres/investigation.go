package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) CodebaseInvestigationSource(ctx context.Context, jobID string) (investigation.Source, error) {
	row, err := dbsql.New(s.DB).GetCodebaseInvestigationSource(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return investigation.Source{}, ErrNotFound
	}
	if err != nil {
		return investigation.Source{}, err
	}
	return investigationSourceFromValues(row.JobID, row.Kind, row.Repository, row.Revision, row.BundleDigest, row.BundleByteSize), nil
}

// resolveInvestigationMessageEnvelope resolves only the investigation execution
// envelope. Generic Message admission owns Follow, Steer, and Thread binding.
func resolveInvestigationMessageEnvelope(ctx context.Context, queries *dbsql.Queries, job dbsql.GetJobAdmissionForUpdateRow, input core.MessageAdmission) (admittedAgentRun, error) {
	if input.SandboxID != core.MainSandboxName(input.JobID) {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation Message requires the workflow-authorized default Sandbox")
	}
	source, err := queries.GetCodebaseInvestigationSource(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	return admittedAgentRun{
		Role: investigation.InitialAgentRole, Capability: investigation.InitialAgentCapability, InputRevision: source.Revision,
		SandboxID: input.SandboxID,
	}, nil
}

func (s Store) CodebaseInvestigationMessages(ctx context.Context, jobID string) ([]investigation.MessageRecord, error) {
	deliveries, err := s.Deliveries(ctx, jobID)
	if err != nil {
		return nil, err
	}
	work := make([]investigation.MessageRecord, 0, len(deliveries))
	for _, delivery := range deliveries {
		run := delivery.AgentRun
		if run.Role != investigation.InitialAgentRole {
			continue
		}
		work = append(work, investigation.MessageRecord{
			MessageID: delivery.Message.ID, Sequence: delivery.Message.Sequence, SandboxID: run.SandboxID,
			Outcome: agentRunOutcome(run.State, run.TurnOutcome), Attention: run.Attention,
		})
	}
	return work, nil
}
