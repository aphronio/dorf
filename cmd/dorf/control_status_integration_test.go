package main

import (
	"context"
	"fmt"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"net/http"
	"reflect"
	"testing"
	"time"
)

type statusControlRuntime struct {
	profile string
	calls   int
	owned   core.Sandbox
}

func (r *statusControlRuntime) ResolveSandbox(_ context.Context, profile core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if profile.Name != r.profile {
		return core.SandboxRuntime{}, fmt.Errorf("foreign profile")
	}
	return core.SandboxRuntime{SandboxProfile: profile, Status: r}, nil
}
func (r *statusControlRuntime) ReadSandboxStatus(_ context.Context, session core.Session, owned core.Sandbox) (provider.Status, error) {
	if owned != r.owned || session.ID != owned.SessionID {
		return provider.Status{}, fmt.Errorf("foreign custody")
	}
	r.calls++
	return provider.Status{Provider: "e2b", State: "paused"}, nil
}
func TestControlStatusUsesPostgresCustodyAndLeavesSessionUnchanged(t *testing.T) {
	ctx := context.Background()
	store, tasks, profile := controlTestStore(t)
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueKey(ctx, "status-client", credential); err != nil {
		t.Fatal(err)
	}
	runtime := &statusControlRuntime{profile: profile}
	handler := controlTestHandler(store, tasks, controlTestGateway(t), auth, runtime)
	var session controlapi.Session
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, "/v1/sessions", credential, fmt.Sprintf("status-%d", time.Now().UnixNano()), controlapi.CreateSessionRequest{AIConnection: "primary", Model: "model-test", Reasoning: "high"}), 201, &session)
	owned, err := store.Sandbox(ctx, core.MainSandboxName(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	runtime.owned = owned
	path := "/v1/sandboxes/" + owned.ID + "/status"
	for _, test := range []struct {
		path, credential string
		code             int
	}{{path, "", 401}, {"/v1/sandboxes/absent/status", credential, 404}, {path + "?provider=foreign", credential, 400}} {
		response := controlTestRequest(t, handler, http.MethodGet, test.path, test.credential, "", nil)
		if response.Code != test.code || runtime.calls != 0 {
			t.Fatalf("status=%d calls=%d", response.Code, runtime.calls)
		}
	}
	before, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result provider.Status
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodGet, path, credential, "", nil), 200, &result)
	after, err := store.Session(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "paused" || result.Provider != "e2b" || !reflect.DeepEqual(before, after) {
		t.Fatalf("observation changed Session or lost status: %+v", result)
	}
	if err := store.ScheduleCleanup(ctx, tasks.QueueName(), session.ID, ""); err != nil {
		t.Fatal(err)
	}
	response := controlTestRequest(t, handler, http.MethodGet, path, credential, "", nil)
	if response.Code != 503 || runtime.calls != 1 {
		t.Fatalf("cleanup status=%d calls=%d", response.Code, runtime.calls)
	}
}
