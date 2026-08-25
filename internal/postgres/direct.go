package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) AdmitDirect(ctx context.Context, input core.JobAdmission) (core.Job, bool, error) {
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	if input.Workflow != "" || input.WorkflowRevision != "" {
		return core.Job{}, false, fmt.Errorf("direct admission cannot use workflow identity")
	}
	normalized, err := normalizeCoreAdmission(input)
	if err != nil {
		return core.Job{}, false, err
	}
	return admitJob(ctx, s, normalized, func(ctx context.Context, queries *dbsql.Queries, ids admittedJobIDs) error {
		_, err := queries.InsertAdmittedAgentRun(ctx, dbsql.InsertAdmittedAgentRunParams{
			ID: core.AgentRunID(ids.messageID), JobID: ids.jobID, MessageID: ids.messageID,
			Role: direct.DirectAgentRole, SandboxID: ids.sandboxID,
		})
		return err
	})
}

func (s Store) AdmitDirectMessage(ctx context.Context, input core.MessageAdmission) (core.MessageAdmissionResult, error) {
	return s.admitMessage(ctx, input, "", "", resolveDirectMessageEnvelope)
}

// resolveDirectMessageEnvelope supplies only the direct execution envelope. Generic
// Message admission owns Follow, Steer, ordering, and Thread binding.
func resolveDirectMessageEnvelope(_ context.Context, _ *dbsql.Queries, _ dbsql.GetJobAdmissionForUpdateRow, input core.MessageAdmission) (admittedAgentRun, error) {
	if input.SandboxID != core.MainSandboxName(input.JobID) {
		return admittedAgentRun{}, fmt.Errorf("direct Message requires the exact default Sandbox")
	}
	return admittedAgentRun{Role: direct.DirectAgentRole, SandboxID: input.SandboxID}, nil
}
