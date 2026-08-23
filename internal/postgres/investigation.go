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

// authorizeInvestigationMessage implements the investigation workflow's
// follow-up policy inside the same locked transaction that records the Message.
func authorizeInvestigationMessage(ctx context.Context, queries *dbsql.Queries, job dbsql.GetJobAdmissionForUpdateRow, input core.MessageAdmission) (admittedAgentRun, error) {
	if input.Intent != core.MessageFollow {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation accepts only follow-up Messages")
	}
	if input.SandboxID != core.MainSandboxName(input.JobID) {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation Message requires the workflow-authorized default Sandbox")
	}
	latest, err := queries.GetLatestInvestigationRun(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	if latest.State != core.AgentRunCompleted || latest.Harness == "" || latest.ThreadID == "" {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation accepts a follow-up only after its latest run completes on a retained Thread")
	}
	source, err := queries.GetCodebaseInvestigationSource(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	return admittedAgentRun{
		Role: investigation.InitialAgentRole, Capability: investigation.InitialAgentCapability, InputRevision: source.Revision,
		SandboxID: input.SandboxID, Harness: latest.Harness, ThreadID: latest.ThreadID,
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
			MessageID: delivery.Message.ID, SandboxID: run.SandboxID,
			Outcome: agentRunOutcome(run.State, run.TurnOutcome), Attention: run.Attention,
		})
	}
	return work, nil
}
