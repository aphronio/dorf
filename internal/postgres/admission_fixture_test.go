package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres"
)

// Existing custody proofs start with the persisted, unscheduled shape written
// by older releases. Manufacture it only in fixtures; production admission now
// always includes scheduling. Public admission tests exercise the atomic path.
func legacyAdmissionFixture(t *testing.T, store postgres.Store, ctx context.Context, admit func(string) (core.Job, bool, error)) (core.Job, bool, error) {
	t.Helper()
	client := newFaultClient(t, store, fmt.Sprintf("dorf_legacy_fixture_%d", time.Now().UnixNano()))
	job, created, err := admit(client.QueueName())
	if err != nil || !job.AdmissionOpen || job.CurrentTaskID == "" {
		return job, created, err
	}
	if err := client.CancelTask(ctx, client.QueueName(), job.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `delete from dorf.job_tasks where job_id=$1 and task_id=$2`, job.ID, job.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	job.CurrentTaskID = ""
	return job, created, nil
}

func admitCodingFixture(t *testing.T, store postgres.Store, ctx context.Context, input coding.Admission) (core.Job, bool, error) {
	return legacyAdmissionFixture(t, store, ctx, func(queue string) (core.Job, bool, error) {
		return store.AdmitCoding(ctx, input, queue)
	})
}

func admitDirectFixture(t *testing.T, store postgres.Store, ctx context.Context, input core.JobAdmission) (core.Job, bool, error) {
	return legacyAdmissionFixture(t, store, ctx, func(queue string) (core.Job, bool, error) {
		return store.AdmitDirect(ctx, input, queue)
	})
}

func admitInvestigationFixture(t *testing.T, store postgres.Store, ctx context.Context, input investigation.Admission) (core.Job, bool, error) {
	return legacyAdmissionFixture(t, store, ctx, func(queue string) (core.Job, bool, error) {
		return store.AdmitInvestigation(ctx, input, queue)
	})
}
