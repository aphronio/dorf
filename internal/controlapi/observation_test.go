package controlapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlauth"
)

type observationSessions struct {
	*fakeSessions
	value          controlapi.MessageObservation
	stopped        chan struct{}
	resume         chan string
	waitBeforeEmit bool
}

func (j *observationSessions) ReadMessageObservation(context.Context, string, string, string) (controlapi.MessageObservation, error) {
	return j.value, nil
}
func (j *observationSessions) StreamMessageObservation(ctx context.Context, _, _, cursor string, emit func(controlapi.MessageObservation) error) error {
	defer close(j.stopped)
	j.resume <- cursor
	if j.waitBeforeEmit {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := emit(j.value); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestMessageObservationStreamRemainsAfterCompleteAndCancelsAtCredentialExpiry(t *testing.T) {
	const credential = "dcr_observation-test"
	outcome, cursor := "completed", "opaque-resume"
	watermark := 2
	sessions := &observationSessions{fakeSessions: &fakeSessions{}, value: controlapi.MessageObservation{State: "complete", Outcome: &outcome, Cursor: &cursor, NextIndex: 2, CompletionWatermark: &watermark, Items: []controlapi.MessageTimelineItem{}}, stopped: make(chan struct{}), resume: make(chan string, 1)}
	auth := &fakeAuth{credential: credential, client: controlauth.Client{CredentialExpiresAt: time.Now().Add(150 * time.Millisecond)}}
	server := controlapi.NewServer(controlapi.Discovery{}, auth, sessions, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session/messages/message/observation/stream", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Last-Event-ID", cursor)
	response := newStreamResponse()
	done := make(chan struct{})
	go func() { server.Handler.ServeHTTP(response, request); close(done) }()
	response.awaitFlush(t)
	if got := <-sessions.resume; got != cursor {
		t.Fatalf("resume=%q", got)
	}
	if body := string(response.bytes()); !strings.Contains(body, "event: observation\nid: "+cursor) || !strings.Contains(body, `"completion_watermark":2`) {
		t.Fatalf("frame=%s", body)
	}
	select {
	case <-done:
		t.Fatal("completion closed the stream before credential expiry")
	default:
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream outlived credential")
	}
	select {
	case <-sessions.stopped:
	case <-time.After(time.Second):
		t.Fatal("worker callback was not cancelled")
	}
}

func TestMessageObservationRejectsMalformedQueriesAndUnauthenticatedStreams(t *testing.T) {
	const credential = "dcr_observation-test"
	sessions := &observationSessions{fakeSessions: &fakeSessions{}}
	server := controlapi.NewServer(controlapi.Discovery{}, &fakeAuth{credential: credential}, sessions, nil)
	for _, target := range []string{"?other=x", "?cursor=a&cursor=b", "?cursor="} {
		request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session/messages/message/observation"+target, nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		server.Handler.ServeHTTP(response, request)
		requireProblem(t, response, http.StatusBadRequest, "invalid_cursor")
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session/messages/message/observation/stream?cursor=a", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Last-Event-ID", "b")
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	requireProblem(t, response, http.StatusBadRequest, "invalid_cursor")
	request = httptest.NewRequest(http.MethodGet, "/v1/sessions/session/messages/message/observation/stream", nil)
	response = httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	requireProblem(t, response, http.StatusUnauthorized, "unauthenticated")
}

func TestMessageObservationReturnsAuthenticationProblemIfExpiryPrecedesFirstFrame(t *testing.T) {
	const credential = "dcr_observation-expiry"
	sessions := &observationSessions{fakeSessions: &fakeSessions{}, waitBeforeEmit: true, stopped: make(chan struct{}), resume: make(chan string, 1)}
	auth := &fakeAuth{credential: credential, client: controlauth.Client{CredentialExpiresAt: time.Now().Add(25 * time.Millisecond)}}
	server := controlapi.NewServer(controlapi.Discovery{}, auth, sessions, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/session/messages/message/observation/stream", nil)
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	requireProblem(t, response, http.StatusUnauthorized, "unauthenticated")
}
