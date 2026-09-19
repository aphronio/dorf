package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

// admitDirectFixture detaches the admission task so custody and fault tests can
// attach and claim their own task. API tests exercise ordinary atomic scheduling.
func admitDirectFixture(t *testing.T, store postgres.Store, ctx context.Context, input core.SessionAdmission) (core.Session, bool, error) {
	t.Helper()
	client := newFaultClient(t, store, fmt.Sprintf("dorf_custody_fixture_%d", time.Now().UnixNano()))
	session, created, err := store.AdmitDirect(ctx, input, client.QueueName())
	if err != nil || !session.AdmissionOpen || session.CurrentTaskID == "" {
		return session, created, err
	}
	if err := client.CancelTask(ctx, client.QueueName(), session.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `delete from dorf.session_tasks where session_id=$1 and task_id=$2`, session.ID, session.CurrentTaskID); err != nil {
		t.Fatal(err)
	}
	session.CurrentTaskID = ""
	session.CurrentTaskName = ""
	return session, created, nil
}

// requestCleanupFixture stops admission without scheduling so fault tests can
// inject and claim an exact cleanup task themselves.
func requestCleanupFixture(ctx context.Context, store postgres.Store, sessionID string) error {
	_, err := store.DB.ExecContext(ctx, `update dorf.sessions set admission_open=false,cleanup_state='requested' where id=$1 and cleanup_state='pending'`, sessionID)
	return err
}

// attachTaskFixture supplies the synthetic task identity used by custody and
// fault tests. Production scheduling commits spawn and attachment together.
func attachTaskFixture(store postgres.Store, ctx context.Context, sessionID, taskID, taskName string) error {
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `insert into dorf.session_tasks(session_id,sequence,task_id,task_name)
select $1,coalesce(max(sequence),0)+1,$2,$3 from dorf.session_tasks where session_id=$1`, sessionID, taskID, taskName)
	if err != nil {
		return err
	}
	if taskName == core.CleanupTaskName {
		if _, err := tx.ExecContext(ctx, `update dorf.sessions set cleanup_state='scheduled' where id=$1`, sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
