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
		Role: "investigate", Capability: "repository-read-report", InputRevision: source.Revision,
		SandboxID: input.SandboxID, Harness: latest.Harness, ThreadID: latest.ThreadID,
	}, nil
}

func (s Store) CodebaseInvestigationMessages(ctx context.Context, jobID string) ([]investigation.MessageRecord, error) {
	rows, err := dbsql.New(s.DB).ListCodebaseInvestigationMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}
	work := make([]investigation.MessageRecord, 0, len(rows))
	for _, row := range rows {
		work = append(work, investigation.MessageRecord{MessageID: row.MessageID, SandboxID: row.SandboxID, Outcome: agentRunOutcome(row.State, row.TurnOutcome), Attention: row.Attention})
	}
	return work, nil
}

func (s Store) ValidateInvestigationAgentMessage(ctx context.Context, execution core.AgentMessageExecution) error {
	source, err := s.CodebaseInvestigationSource(ctx, execution.Job.ID)
	if err != nil {
		return err
	}
	if execution.AgentRun.Role != "investigate" || execution.AgentRun.Capability != "repository-read-report" ||
		execution.AgentRun.InputRevision != source.Revision || execution.AgentRun.SandboxID != core.MainSandboxName(execution.Job.ID) {
		return fmt.Errorf("Message %s conflicts with the exact investigation Agent contract", execution.Message.ID)
	}
	messages, err := s.CodebaseInvestigationMessages(ctx, execution.Job.ID)
	if err != nil {
		return err
	}
	for _, message := range messages {
		if message.Outcome != "" || message.Attention != "" {
			continue
		}
		if message.MessageID != execution.Message.ID || message.SandboxID != execution.Sandbox.ID {
			return fmt.Errorf("Message %s is no longer the exact eligible investigation Agent Message", execution.Message.ID)
		}
		return nil
	}
	return fmt.Errorf("Message %s is no longer eligible for investigation Agent reconciliation", execution.Message.ID)
}
