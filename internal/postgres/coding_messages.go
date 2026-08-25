package postgres

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// resolveCodingMessageEnvelope resolves only the coding execution envelope inside
// the same locked transaction that records the accepted Message.
func resolveCodingMessageEnvelope(ctx context.Context, queries *dbsql.Queries, job dbsql.GetJobAdmissionForUpdateRow, input core.MessageAdmission) (admittedAgentRun, error) {
	if input.SandboxID != core.MainSandboxName(input.JobID) {
		return admittedAgentRun{}, fmt.Errorf("coding Message requires the workflow-authorized default Sandbox")
	}
	codingInput, err := queries.GetCodingToProposalInput(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	if !ValidRevision(codingInput.Revision) {
		return admittedAgentRun{}, fmt.Errorf("coding Message requires the locked current Revision")
	}
	return admittedAgentRun{Role: coding.InitialAgentRole, InputRevision: codingInput.Revision, SandboxID: input.SandboxID}, nil
}
