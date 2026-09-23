package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/controlreader"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
)

type observationTestRuntime struct {
	mu       sync.Mutex
	profile  string
	items    []core.HarnessConversationItem
	complete bool
	reads    int
}

func (r *observationTestRuntime) ResolveSandbox(_ context.Context, profile core.SandboxProfileRef) (core.SandboxRuntime, error) {
	if profile.Name != r.profile {
		return core.SandboxRuntime{}, fmt.Errorf("foreign profile")
	}
	return core.SandboxRuntime{SandboxProfile: profile, Timeline: r}, nil
}
func (r *observationTestRuntime) ReadTimeline(_ context.Context, _ core.Session, _ core.Sandbox, thread, turn string) (core.HarnessTimeline, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	status := "inProgress"
	if r.complete {
		status = "completed"
	}
	return core.HarnessTimeline{Harness: "codex", ThreadID: thread, TurnID: turn, Status: status, Items: []json.RawMessage{json.RawMessage(`{"id":"input-0","type":"userMessage"}`)}, CompletedItems: append([]core.HarnessConversationItem{}, r.items...)}, nil
}
func TestControlObservationStreamsAcrossPrivateHTTPWithDurableCustody(t *testing.T) {
	ctx := context.Background()
	store, tasks, profile := controlTestStore(t)
	gateway := controlTestGateway(t)
	auth := controlauth.Service{Store: store}
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = auth.IssueKey(ctx, "observation-integration", credential); err != nil {
		t.Fatal(err)
	}
	runtime := &observationTestRuntime{profile: profile}
	feed := codex.NewReplyFeed()
	service := controlreader.Service{Store: store, Runtimes: runtime, Provider: gateway, Replies: feed}
	privateHandler, err := controlreader.NewHandler(strings.Repeat("d", 64), service)
	if err != nil {
		t.Fatal(err)
	}
	privateServer := httptest.NewServer(privateHandler)
	defer privateServer.Close()
	reader, err := controlreader.NewClient(privateServer.URL, strings.Repeat("d", 64), privateServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	handler := controlapi.NewServer(controlapi.Discovery{Product: "dorf"}, auth, controlAPISessions{store: store, tasks: tasks, directAdmissions: direct.NewAdmissionService(store, tasks.QueueName(), reader), reader: reader}, controlAPIProfiles{store: store}).Handler
	key := fmt.Sprintf("observation-%d", time.Now().UnixNano())
	var session controlapi.Session
	controlTestJSON(t, controlTestRequest(t, handler, http.MethodPost, "/v1/sessions", credential, key+"-session", controlapi.CreateSessionRequest{AIConnection: "primary", Model: "model-test", Reasoning: "high"}), 201, &session)
	runID := "input/dispatch"
	if err = store.BindNativeThread(ctx, session.ID, "native-thread"); err != nil {
		t.Fatal(err)
	}
	owned, err := store.Sandbox(ctx, core.MainSandboxName(session.ID))
	if err != nil {
		t.Fatal(err)
	}
	binding := codex.ReplyBinding{SessionID: session.ID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce, Harness: "codex", ThreadID: "native-thread", TurnID: "native-turn"}
	runtime.items = []core.HarnessConversationItem{{Index: 0, NativeItemID: "input-0", Kind: "input", ClientID: runID}}
	publicServer := httptest.NewServer(handler)
	defer publicServer.Close()
	path := publicServer.URL + "/v1/sessions/" + session.ID + "/turns/native-turn"
	get := func(cursor string) controlapi.TurnObservation {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		query := request.URL.Query()
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		request.URL.RawQuery = query.Encode()
		request.Header.Set("Authorization", "Bearer "+credential)
		response, err := publicServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("observation status=%d", response.StatusCode)
		}
		var value controlapi.TurnObservation
		if err = json.NewDecoder(response.Body).Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	first := get("")
	if first.State != "observing" || first.NextIndex != 1 || len(first.Items) != 1 || first.Items[0].ClientID != "input" || first.Cursor == nil {
		t.Fatalf("snapshot=%+v", first)
	}
	stream := openObservationTestStream(t, publicServer.Client(), publicServer.URL+"/v1/sessions/"+session.ID+"/events/stream?turn_id=native-turn", credential, *first.Cursor)
	initial := stream.next(t)
	if len(initial.Items) != 0 || initial.NextIndex != 1 {
		t.Fatalf("initial=%+v", initial)
	}
	item := core.HarnessConversationItem{Index: 1, NativeItemID: "reply-1", Kind: "assistant_message", Phase: "commentary", Text: "completed reply"}
	runtime.mu.Lock()
	runtime.items = append(runtime.items, item)
	runtime.mu.Unlock()
	feed.Append(binding, item)
	reply := stream.next(t)
	if len(reply.Items) != 1 || reply.Items[0].Kind != "assistant_message" || reply.Items[0].Phase != "commentary" || reply.Items[0].Text != "completed reply" || reply.FromIndex != 1 || reply.NextIndex != 2 || reply.Cursor == nil {
		t.Fatalf("reply=%+v", reply)
	}
	stream.close(t)
	stream = openObservationTestStream(t, publicServer.Client(), publicServer.URL+"/v1/sessions/"+session.ID+"/events/stream?turn_id=native-turn", credential, *reply.Cursor)
	replay := stream.next(t)
	if len(replay.Items) != 0 || replay.NextIndex != 2 {
		t.Fatalf("replay=%+v", replay)
	}
	runtime.mu.Lock()
	runtime.complete = true
	items := append([]core.HarnessConversationItem{}, runtime.items...)
	runtime.mu.Unlock()
	feed.Seed(binding, items, true)
	nativeOnly := get(*reply.Cursor)
	if nativeOnly.State != "observing" || nativeOnly.CompletionWatermark != nil {
		t.Fatalf("premature terminal=%+v", nativeOnly)
	}
	feed.Status(binding, "completed")
	terminal := stream.next(t)
	if terminal.State != "complete" || terminal.Status != "completed" || terminal.CompletionWatermark == nil || *terminal.CompletionWatermark != 2 || len(terminal.Items) != 0 {
		t.Fatalf("terminal=%+v", terminal)
	}
	stream.close(t)
	runtime.mu.Lock()
	reads := runtime.reads
	runtime.mu.Unlock()
	if reads != 1 {
		t.Fatalf("stream performed native reads: got=%d want=1 initial hydration", reads)
	}
	feed.Gap(binding)
	stream = openObservationTestStream(t, publicServer.Client(), publicServer.URL+"/v1/sessions/"+session.ID+"/events/stream?turn_id=native-turn", credential, *terminal.Cursor)
	gap := stream.next(t)
	if gap.State != "gap" || gap.Cursor == nil || *gap.Cursor != *terminal.Cursor || len(gap.Items) != 0 {
		t.Fatalf("gap=%+v", gap)
	}
	stream.close(t)
	repaired := get(*terminal.Cursor)
	if repaired.State != "complete" || repaired.Cursor == nil || *repaired.Cursor != *terminal.Cursor || len(repaired.Items) != 0 {
		t.Fatalf("resync=%+v", repaired)
	}
	for _, check := range []struct {
		bearer, cursor string
		status         int
	}{{"", "", 401}, {credential, "invalid", 400}} {
		request, _ := http.NewRequest(http.MethodGet, path, nil)
		if check.bearer != "" {
			request.Header.Set("Authorization", "Bearer "+check.bearer)
		}
		query := request.URL.Query()
		if check.cursor != "" {
			query.Set("cursor", check.cursor)
		}
		request.URL.RawQuery = query.Encode()
		response, err := publicServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != check.status {
			t.Fatalf("invalid request status=%d want=%d", response.StatusCode, check.status)
		}
	}
}

type observationTestStream struct {
	cancel   context.CancelFunc
	response *http.Response
	values   chan controlapi.TurnObservation
	done     chan error
}

func openObservationTestStream(t *testing.T, client *http.Client, path, bearer, cursor string) *observationTestStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "text/event-stream")
	query := request.URL.Query()
	query.Set("cursor", cursor)
	request.URL.RawQuery = query.Encode()
	response, err := client.Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		cancel()
		response.Body.Close()
		t.Fatalf("stream status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	stream := &observationTestStream{cancel: cancel, response: response, values: make(chan controlapi.TurnObservation, 8), done: make(chan error, 1)}
	t.Cleanup(func() { cancel(); response.Body.Close() })
	go func() {
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var value controlapi.TurnObservation
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &value); err != nil {
				stream.done <- err
				return
			}
			select {
			case stream.values <- value:
			case <-ctx.Done():
				stream.done <- ctx.Err()
				return
			}
		}
		stream.done <- scanner.Err()
	}()
	return stream
}
func (s *observationTestStream) next(t *testing.T) controlapi.TurnObservation {
	t.Helper()
	select {
	case value := <-s.values:
		return value
	case err := <-s.done:
		t.Fatalf("stream closed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("stream failed to flush frame")
	}
	return controlapi.TurnObservation{}
}
func (s *observationTestStream) close(t *testing.T) {
	t.Helper()
	s.cancel()
	s.response.Body.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream cancellation did not release reader")
	}
}
