package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// ListedJob is the narrow durable identity needed by the remote Job index.
// Full Job state remains available through the canonical single-Job read.
type ListedJob struct {
	ID               string
	Workflow         core.WorkflowName
	WorkflowRevision string
	AdmittedAt       time.Time
}

// ListSupportedJobs returns current public Job identities strictly before the
// optional immutable (admitted_at,id) position.
func (s Store) ListSupportedJobs(ctx context.Context, limit int, cursorAt time.Time, cursorID string) ([]ListedJob, error) {
	if limit < 1 || limit > 101 {
		return nil, fmt.Errorf("Job list limit must be between 1 and 101")
	}
	if (cursorID == "") != cursorAt.IsZero() {
		return nil, fmt.Errorf("Job list cursor requires both admitted time and Job ID")
	}
	rows, err := dbsql.New(s.DB).ListSupportedJobs(ctx, dbsql.ListSupportedJobsParams{
		CodingWorkflow:          string(coding.Workflow),
		CodingRevision:          coding.WorkflowRevision,
		InvestigationWorkflow:   string(investigation.Workflow),
		InvestigationRevision:   investigation.WorkflowRevision,
		HasCursor:               cursorID != "",
		CursorAdmittedAt:        cursorAt,
		CursorID:                cursorID,
		PageSize:                int32(limit),
	})
	if err != nil {
		return nil, err
	}
	jobs := make([]ListedJob, 0, len(rows))
	for _, row := range rows {
		jobs = append(jobs, ListedJob{
			ID: row.ID, Workflow: row.WorkflowName, WorkflowRevision: row.WorkflowRevision, AdmittedAt: row.AdmittedAt,
		})
	}
	return jobs, nil
}
