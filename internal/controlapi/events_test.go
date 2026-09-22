package controlapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type nativeSessions struct {
	*fakeSessions
	calls int
	event core.NativeEvent
	err   error
}

func (s *nativeSessions) SubmitEvent(_ context.Context, id string, event core.NativeEvent) (core.NativeAcknowledgement, error) {
	s.calls++
	s.event = event
	return core.NativeAcknowledgement{Type: "input.accepted", Harness: "codex", ClientID: event.ClientID, ThreadID: "thread", TurnID: "turn"}, s.err
}
func (s *nativeSessions) ReadNativeTurns(context.Context, string) (core.HarnessHistory, error) {
	return core.HarnessHistory{Harness: "codex", ThreadID: "thread", Turns: []core.HarnessTurn{{ID: "turn", Status: "inProgress"}}}, nil
}
func TestEventsAcknowledgeNativeInputAndExposeUncertainty(t *testing.T) {
	service := &nativeSessions{fakeSessions: &fakeSessions{}}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: "dcr_native"}, service, nil).Handler
	send := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/sessions/session/events", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer dcr_native")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	body := `{"type":"input.message","client_id":"client","text":"hello","attachments":[{"filename":"note.txt","contents":"aGVsbG8="}]}`
	for i := 0; i < 2; i++ {
		w := send(body)
		requireStatusType(t, w, 200, "application/json")
		if !strings.Contains(w.Body.String(), `"turn_id":"turn"`) {
			t.Fatal(w.Body.String())
		}
	}
	if service.calls != 2 || string(service.event.Attachments[0].Contents) != "hello" {
		t.Fatalf("calls=%d event=%+v", service.calls, service.event)
	}
	service.err = core.ErrNativeUnknown
	requireProblem(t, send(body), http.StatusConflict, "native_outcome_unknown")
	if service.calls != 3 {
		t.Fatal("uncertain native mutation was retried")
	}
	requireProblem(t, send(`{"type":"input.message","text":"missing correlation"}`), http.StatusUnprocessableEntity, "invalid_input")
}

func TestNativeEventAttachmentBoundaries(t *testing.T) {
	service := &nativeSessions{fakeSessions: &fakeSessions{}}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: "dcr_native"}, service, nil).Handler
	send := func(event core.NativeEvent, knownLength bool) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/sessions/session/events", bytes.NewReader(body))
		if !knownLength {
			r.ContentLength = -1
		}
		r.Header.Set("Authorization", "Bearer dcr_native")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	event := core.NativeEvent{Type: core.InputMessage, ClientID: "ten-files"}
	for i := range 10 {
		event.Attachments = append(event.Attachments, core.NativeAttachment{Filename: fmt.Sprintf("file-%02d.txt", i), Contents: []byte(fmt.Sprintf("contents %d", i))})
	}
	// Attachment-only input preserves every filename, byte and ordinal.
	requireStatusType(t, send(event, true), http.StatusOK, "application/json")
	if service.calls != 1 || !reflect.DeepEqual(service.event, event) {
		t.Fatal("ten-file event did not reach the service unchanged")
	}
	event.Attachments = append(event.Attachments, core.NativeAttachment{Filename: "eleven.txt"})
	requireProblem(t, send(event, true), http.StatusUnprocessableEntity, "invalid_input")
	event.Attachments = event.Attachments[:10]
	event.Attachments[9].Contents = make([]byte, provider.MaxFileWriteBytes+1)
	requireProblem(t, send(event, true), http.StatusUnprocessableEntity, "invalid_input")
	event.Attachments[9].Contents = make([]byte, provider.MaxFileWriteBytes)
	requireStatusType(t, send(event, true), http.StatusOK, "application/json")
	if !reflect.DeepEqual(service.event, event) {
		t.Fatal("maximum-sized file changed in transit")
	}
	// Individually valid files must still fit the unchanged 46 MiB body budget.
	for i := range event.Attachments {
		event.Attachments[i].Contents = make([]byte, 4<<20)
	}
	for _, knownLength := range []bool{true, false} {
		requireProblem(t, send(event, knownLength), http.StatusRequestEntityTooLarge, "body_too_large")
	}
	if service.calls != 2 {
		t.Fatalf("rejected input reached the service: calls=%d", service.calls)
	}
}

func (s *nativeSessions) InputCapabilities(context.Context, string) (core.InputCapabilities, error) {
	return core.InputCapabilities{Model: "model", AudioMediaTypes: []string{}}, nil
}

func TestInputCapabilitiesRequireAuthentication(t *testing.T) {
	service := &nativeSessions{fakeSessions: &fakeSessions{}}
	handler := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: "dcr_native"}, service, nil).Handler
	for _, authenticated := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session/input-capabilities", nil)
		if authenticated {
			request.Header.Set("Authorization", "Bearer dcr_native")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if !authenticated {
			if response.Code != http.StatusUnauthorized {
				t.Fatal(response.Code)
			}
			continue
		}
		requireStatusType(t, response, http.StatusOK, "application/json")
		if !strings.Contains(response.Body.String(), `"audio_media_types":[]`) || !strings.Contains(response.Body.String(), `"model":"model"`) {
			t.Fatal(response.Body.String())
		}
	}
}
