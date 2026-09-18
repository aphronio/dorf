package postgres_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
)

// Existing custody proofs start with the persisted, unscheduled shape written
// by older releases. Manufacture it only in fixtures; production admission now
// always includes scheduling. Public admission tests exercise the atomic path.
func legacyAdmissionFixture(t *testing.T, store postgres.Store, ctx context.Context, admit func(string) (core.Session, bool, error)) (core.Session, bool, error) {
	t.Helper()
	client := newFaultClient(t, store, fmt.Sprintf("dorf_legacy_fixture_%d", time.Now().UnixNano()))
	session, created, err := admit(client.QueueName())
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
	return session, created, nil
}

func admitDirectFixture(t *testing.T, store postgres.Store, ctx context.Context, input core.SessionAdmission) (core.Session, bool, error) {
	return legacyAdmissionFixture(t, store, ctx, func(queue string) (core.Session, bool, error) {
		session, created, err := store.AdmitDirect(ctx, input, queue)
		if err == nil {
			_, err = store.AdmitDirectMessage(ctx, fixtureMessage(session.ID))
		}
		return session, created, err
	})
}

func fixtureMessage(sessionID string) core.MessageAdmission {
	return core.MessageAdmission{SessionID: sessionID, SandboxID: core.MainSandboxName(sessionID), FromKind: core.MessageFromHuman, FromID: "fixture-message", Input: "initial input", Intent: core.MessageFollow}
}
