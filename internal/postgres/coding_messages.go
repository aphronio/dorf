package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// authorizeCodingMessage implements the coding workflow's delivery policy
// inside the same locked transaction that records the accepted Message.
func authorizeCodingMessage(ctx context.Context, queries *dbsql.Queries, job dbsql.GetJobAdmissionForUpdateRow, input core.MessageAdmission) (admittedAgentRun, error) {
	if job.OutcomeExists {
		return admittedAgentRun{}, fmt.Errorf("Job %s outcome is already recorded", input.JobID)
	}
	run := admittedAgentRun{Role: "implement", SandboxID: core.MainSandboxName(input.JobID)}
	if input.Intent == core.MessageSteer {
		active, err := queries.GetActiveImplementationTurn(ctx, input.JobID)
		if errors.Is(err, sql.ErrNoRows) {
			return admittedAgentRun{}, fmt.Errorf("steer delivery requires an exact active regular harness Turn")
		}
		if err != nil {
			return admittedAgentRun{}, err
		}
		run.TargetTurnID, run.Harness, run.ThreadID, run.Role = active.TurnID, active.Harness, active.ThreadID, active.Role
		return run, nil
	}
	prior, err := queries.GetLatestImplementationThreadBinding(ctx, input.JobID)
	if err == nil {
		run.Harness, run.ThreadID = prior.Harness, prior.ThreadID
	} else if !errors.Is(err, sql.ErrNoRows) {
		return admittedAgentRun{}, err
	}
	return run, nil
}
