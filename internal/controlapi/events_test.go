package controlapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
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
