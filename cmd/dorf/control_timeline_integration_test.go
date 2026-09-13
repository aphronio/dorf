package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
)

type timelineControlRuntime struct {
	profile         string
	calls           int
	expectedJob     string
	expectedSandbox core.Sandbox
	block           chan struct{}
	entered         chan struct{}
	sourceRun       string
}

func (r *timelineControlRuntime) ResolveSandbox(_ context.Context, profile string) (core.SandboxRuntime, error) {
	if profile != r.profile {
		return core.SandboxRuntime{}, fmt.Errorf("foreign profile")
	}
	return core.SandboxRuntime{SandboxProfile: profile, Timeline: r}, nil
}
func (r *timelineControlRuntime) ReadTimeline(ctx context.Context, job core.Job, owned core.Sandbox, threadID, turnID string) (core.HarnessTimeline, error) {
	r.calls++
	if job.ID != r.expectedJob || owned != r.expectedSandbox || threadID != "native-thread" {
		return core.HarnessTimeline{}, fmt.Errorf("wrong persisted custody")
	}
	if turnID == "missing" {
		return core.HarnessTimeline{}, core.ErrTurnNotFound
	}
	if r.block != nil {
		close(r.entered)
		select {
		case <-r.block:
		case <-ctx.Done():
			return core.HarnessTimeline{}, ctx.Err()
		}
	}
	if turnID == "" {
		turnID = "latest-native-turn"
	}
	return core.HarnessTimeline{Harness: "codex", ThreadID: threadID, TurnID: turnID, Status: "inProgress", CompletedItems: []core.HarnessConversationItem{{Index: 0, NativeItemID: "native-input", Kind: "input", ClientID: r.sourceRun}, {Index: 1, NativeItemID: "native-final", Kind: "reply", Text: "[PDF](sandbox:/report.pdf)"}}, Items: []json.RawMessage{json.RawMessage(`{"type":"agentMessage","id":"native-item","phase":"commentary","text":"Still working"}`)}}, nil
}

func TestControlTimelineUsesPostgresCustodyAndCleanupFence(t *testing.T) {
	ctx := context.Background()
	store, tasks, profile := controlTestStore(t)
	gateway := controlTestGateway(t)
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.IssueKey(ctx, "timeline-client", credential); err != nil {
		t.Fatal(err)
	}
	runtime := &timelineControlRuntime{profile: profile}
	handler := controlTestHandler(store, tasks, gateway, auth, runtime, blob.Store{Root: t.TempDir()})
	key := fmt.Sprintf("timeline-%d", time.Now().UnixNano())
	var job controlapi.DirectJob
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, "/v1/jobs", credential, key, controlapi.AdmitJobRequest{AIConnection: "primary", Model: "model-test", Reasoning: "high"}), http.StatusCreated, &job)
	path := "/v1/jobs/" + job.ID + "/timeline"
	problemCode := func(path, credential string, status int, code string) {
		t.Helper()
		var problem controlapi.Problem
		controlTestJSON(t, controlTestRequest(t, handler, http.MethodGet, path, credential, "", nil), status, &problem)
		if problem.Code != code {
			t.Fatalf("problem=%+v", problem)
		}
	}
	problemCode(path, "", 401, "unauthenticated")
	problemCode("/v1/jobs/no-such-job/timeline", credential, 404, "job_not_found")
	problemCode(path, credential, 409, "timeline_unavailable")
	for _, query := range []string{"?turn_id=", "?turn_id=one&turn_id=two", "?thread_id=foreign"} {
		problemCode(path+query, credential, 400, "invalid_query")
	}
	if runtime.calls != 0 {
		t.Fatal("unbound or unauthorized read reached native reader")
	}
	var message controlapi.Message
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, "/v1/jobs/"+job.ID+"/messages", credential, key+"-message", controlapi.SendMessageRequest{Text: "Retained work"}), http.StatusCreated, &message)
	messagePath := "/v1/jobs/" + job.ID + "/messages/" + message.ID + "/timeline"
	problemCode(messagePath, "", 401, "unauthenticated")
	problemCode(messagePath, credential, 409, "timeline_unavailable")
	problemCode("/v1/jobs/"+job.ID+"/messages/not-ours/timeline", credential, 404, "message_not_found")
	for _, query := range []string{"?turn_id=foreign", "?message_id=other"} {
		problemCode(messagePath+query, credential, 400, "invalid_query")
	}
	var denied controlapi.Problem
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, messagePath, credential, "", nil), 405, &denied)
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodGet, messagePath, credential, "", map[string]string{"input": "forbidden"}), 415, &denied)
	runID := core.AgentRunID(message.ID)
	runtime.sourceRun = runID
	if err := store.PrepareAgentRun(ctx, runID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, runID, "codex", "native-thread", "native-turn", "inProgress"); err != nil {
		t.Fatal(err)
	}
	owned, err := store.Sandbox(ctx, core.MainSandboxName(job.ID))
	if err != nil {
		t.Fatal(err)
	}
	runtime.expectedJob, runtime.expectedSandbox = job.ID, owned
	before, err := store.Deliveries(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeJob, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var messageTimeline controlapi.MessageTimeline
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodGet, messagePath, credential, "", nil), 200, &messageTimeline)
	if messageTimeline.MessageID != message.ID || messageTimeline.JobID != job.ID || messageTimeline.TurnID != "native-turn" || messageTimeline.Status != "inProgress" || len(messageTimeline.Items) != 2 || messageTimeline.Items[0].MessageID != message.ID || messageTimeline.Items[0].Text != nil || messageTimeline.Items[1].Text == nil || *messageTimeline.Items[1].Text != "[PDF](sandbox:/report.pdf)" {
		t.Fatalf("message timeline=%+v", messageTimeline)
	}
	var timeline controlapi.Timeline
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodGet, path, credential, "", nil), 200, &timeline)
	if timeline.JobID != job.ID || timeline.TurnID != "latest-native-turn" || timeline.ThreadID != "native-thread" || len(timeline.Items) != 1 {
		t.Fatalf("timeline=%+v", timeline)
	}
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodGet, path+"?turn_id=old-native-turn", credential, "", nil), 200, &timeline)
	if timeline.TurnID != "old-native-turn" {
		t.Fatalf("explicit turn=%+v", timeline)
	}
	problemCode(path+"?turn_id=missing", credential, 404, "turn_not_found")
	after, err := store.Deliveries(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterJob, err := store.Job(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeJob, afterJob) {
		t.Fatal("timeline read changed retained work")
	}
	runtime.block, runtime.entered = make(chan struct{}), make(chan struct{})
	readDone := make(chan int, 1)
	go func() {
		readDone <- controlTestRequest(t, handler, http.MethodGet, messagePath, credential, "", nil).Code
	}()
	<-runtime.entered
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- store.ScheduleCleanup(ctx, tasks.QueueName(), job.ID, "") }()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup crossed active read fence: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(runtime.block)
	if status := <-readDone; status != 200 {
		t.Fatalf("active read status=%d", status)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	calls := runtime.calls
	problemCode(messagePath, credential, 409, "timeline_unavailable")
	problemCode(path, credential, 409, "timeline_unavailable")
	if runtime.calls != calls {
		t.Fatal("read after cleanup reached native reader")
	}
}
