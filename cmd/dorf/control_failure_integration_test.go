package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestControlAPIProjectsPersistedSandboxCreationFailure(t *testing.T) {
	tests := []struct {
		name    string
		failure string
		code    string
		detail  string
	}{
		{
			name:    "VM capacity",
			failure: `create Incus instance private-sandbox: Reached maximum number of instances of type "virtual-machine" in project "private-project"`,
			code:    "sandbox_capacity_exhausted",
			detail:  "Sandbox creation failed because the VM limit was reached. Free capacity or increase the limit, then retry.",
		},
		{
			name:    "instance capacity after diagnostic truncation",
			failure: strings.Repeat("private-context ", 40) + `create Incus instance private-sandbox: Reached maximum number of instances in project "private-project"`,
			code:    "sandbox_capacity_exhausted",
			detail:  "Sandbox creation failed because the VM limit was reached. Free capacity or increase the limit, then retry.",
		},
		{
			name:    "unknown provider failure",
			failure: `create Incus instance private-sandbox: unrelated failure for private-project`,
			code:    "execution_failed",
			detail:  "Session execution stopped; inspect the deployment service logs, repair the cause, then retry.",
		},
		{
			name:    "capacity phrase outside creation",
			failure: `execute command: Reached maximum number of instances in project "private-project"`,
			code:    "execution_failed",
			detail:  "Session execution stopped; inspect the deployment service logs, repair the cause, then retry.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, tasks, profile := controlTestStore(t)
			auth := controlauth.Service{Store: store}
			credential, err := controlauth.GenerateCredential()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := auth.IssueKey(ctx, "failure-client", credential); err != nil {
				t.Fatal(err)
			}
			handler := controlTestHandler(store, tasks, controlTestGateway(t), auth, controlTestRuntimes{profile: profile})
			var admitted controlapi.Session
			controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, "/v1/sessions", credential, profile, controlapi.CreateSessionRequest{
				AgentsMD: "failure integration", AIConnection: "primary", Model: "model-test",
			}), http.StatusCreated, &admitted)
			session, err := store.Session(ctx, admitted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := tasks.CancelTask(ctx, tasks.QueueName(), session.CurrentTaskID); err != nil {
				t.Fatal(err)
			}
			const taskName = "sandbox-creation-failure-proof"
			tasks.MustRegister(absurd.Task(taskName, func(context.Context, core.SessionTaskParams) (core.TaskResultV1, error) {
				return core.TaskResultV1{}, errors.New(test.failure + " secret-token-marker")
			}))
			failed, err := tasks.Spawn(ctx, taskName, core.SessionTaskParams{SessionID: session.ID}, absurd.SpawnOptions{MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.AttachSessionTask(ctx, session.ID, session.CurrentTaskID, failed.TaskID, taskName); err != nil {
				t.Fatal(err)
			}
			if err := tasks.WorkBatch(ctx, absurd.WorkBatchOptions{WorkerID: "failure-proof", BatchSize: 1, ClaimTimeout: time.Minute}); err != nil {
				t.Fatal(err)
			}
			persisted, err := tasks.FetchTaskResult(ctx, tasks.QueueName(), failed.TaskID)
			if err != nil || persisted == nil || persisted.State != absurd.TaskFailed {
				t.Fatalf("persisted failure=%#v err=%v", persisted, err)
			}
			response := controlTestRequest(t, handler, http.MethodGet, "/v1/sessions/"+session.ID, credential, "", nil)
			var view controlapi.Session
			controlTestJSON(t, response, http.StatusOK, &view)
			if view.Execution.State != "failed" || view.Attention == nil || view.Attention.Code != test.code || view.Attention.Detail != test.detail {
				t.Fatalf("execution=%q attention=%#v", view.Execution.State, view.Attention)
			}
			for _, private := range []string{"private-sandbox", "private-project", "private-context", "secret-token-marker", "traceback"} {
				if strings.Contains(response.Body.String(), private) {
					t.Fatalf("Session response exposed %q", private)
				}
			}
		})
	}
}
