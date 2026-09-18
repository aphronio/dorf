package controlclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/hostclientconfig"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestLoopbackClientCannotLeakBearerToProxyRedirectOrAlternateOrigin(t *testing.T) {
	const credential = "loopback-client-secret"
	var alternateRequests atomic.Int32
	alternate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		alternateRequests.Add(1)
		if request.Header.Get("Authorization") != "" {
			t.Errorf("alternate origin received Authorization=%q", request.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer alternate.Close()
	t.Setenv("HTTP_PROXY", alternate.URL)
	t.Setenv("http_proxy", alternate.URL)

	originRequests := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		originRequests++
		if request.Host != "127.0.0.1:8745" || request.URL.Path != "/v1/me" || request.Header.Get("Authorization") != "Bearer "+credential {
			t.Errorf("loopback request host=%q path=%q auth=%q", request.Host, request.URL.Path, request.Header.Get("Authorization"))
		}
		http.Redirect(w, request, alternate.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client, err := NewLoopback(credential)
	if err != nil {
		t.Fatal(err)
	}
	if client.base.String() != hostclientconfig.HostOrigin {
		t.Fatalf("loopback origin=%q", client.base)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("loopback transport=%T", client.http.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("loopback transport consulted a proxy function")
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "localhost:8745"); err == nil {
		t.Fatal("loopback transport accepted an alternate origin")
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "127.0.0.1:8745" {
			t.Fatalf("dial destination=%s %s", network, address)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(origin.URL, "http://"))
	}

	if _, err := client.Me(context.Background()); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if originRequests != 1 || alternateRequests.Load() != 0 {
		t.Fatalf("origin requests=%d alternate requests=%d", originRequests, alternateRequests.Load())
	}
}

func TestProblemsRedirectsAndOversizedResponsesDoNotLeakCredential(t *testing.T) {
	const credential = "never-print-this-credential"
	escapedGoal := strings.Repeat("\x00", 1<<20)
	escapedSession, err := json.Marshal(controlapi.Session{ID: "job-1", Attention: &controlapi.Attention{Code: "session_attention", Detail: escapedGoal}})
	if err != nil {
		t.Fatal(err)
	}
	if len(escapedSession) <= 2<<20 || len(escapedSession) > maxResponseBytes {
		t.Fatalf("escaped 1 MiB goal response size=%d", len(escapedSession))
	}
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		switch requests {
		case 1:
			response := jsonResponse(http.StatusTemporaryRedirect, "")
			response.Header.Set("Location", "https://other.example.test/v1/me")
			return response, nil
		case 2:
			return jsonResponse(http.StatusUnauthorized, `{"type":"https://dorf.dev/problems/invalid-client","title":"never-print-this-credential","status":401,"code":"invalid_client","retryable":false,"details":{"echo":"never-print-this-credential"}}`), nil
		case 3:
			return jsonResponse(http.StatusOK, string(escapedSession)), nil
		case 4:
			return jsonResponse(http.StatusOK, strings.Repeat("x", maxResponseBytes+1)), nil
		case 5:
			return nil, errors.New("transport echoed " + credential)
		default:
			t.Fatal("redirect was followed or an unexpected request was issued")
			return nil, nil
		}
	})
	client, err := New("https://dorf.example.test", credential, transport)
	if err != nil {
		t.Fatal(err)
	}
	for _, printed := range []string{fmt.Sprintf("%v", client), fmt.Sprintf("%+v", client), fmt.Sprintf("%#v", client)} {
		if strings.Contains(printed, credential) {
			t.Fatalf("printed Client leaked credential: %s", printed)
		}
	}
	if _, err := client.Me(context.Background()); err == nil || strings.Contains(err.Error(), credential) || requests != 1 {
		t.Fatalf("redirect requests=%d err=%v", requests, err)
	}
	_, err = client.Me(context.Background())
	var problem *ProblemError
	if !IsServiceError(err) || !errors.As(err, &problem) || problem.Problem.Code != "invalid_client" || strings.Contains(err.Error(), credential) {
		t.Fatalf("problem=%#v err=%v", problem, err)
	}
	session, err := client.Session(context.Background(), "job-1")
	if err != nil || session.Attention.Detail != escapedGoal {
		t.Fatalf("escaped Session goal length=%d err=%v", len(session.Model), err)
	}
	if _, err := client.Me(context.Background()); err == nil || strings.Contains(err.Error(), credential) {
		t.Fatalf("oversized response err=%v", err)
	}
	if _, err := client.Me(context.Background()); err == nil || !IsServiceError(err) || strings.Contains(err.Error(), credential) {
		t.Fatalf("transport error err=%v", err)
	}

	_, err = client.Session(context.Background(), "")
	if err == nil || IsServiceError(err) {
		t.Fatalf("client validation error=%v was classified as a service error", err)
	}
}

func TestWatchSessionReconnectsWithoutOrdinaryRequestTimeout(t *testing.T) {
	const credential = "watch-credential"
	stop := errors.New("snapshots complete")
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/v1/sessions/job-1/watch" || request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("Authorization") != "Bearer "+credential {
			t.Fatalf("watch request %d = %s %s accept=%q auth=%q", requests, request.Method, request.URL, request.Header.Get("Accept"), request.Header.Get("Authorization"))
		}
		if _, deadline := request.Context().Deadline(); deadline {
			t.Fatal("stream inherited the ordinary 30-second client timeout")
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: -1}
		response.Header.Set("Content-Type", "text/event-stream")
		switch requests {
		case 1:
			if request.Header.Get("Last-Event-ID") != "" {
				t.Fatalf("initial Last-Event-ID=%q", request.Header.Get("Last-Event-ID"))
			}
			response.Body = io.NopCloser(strings.NewReader(": connected\nretry: 0\nevent: snapshot\nid: snapshot-1\ndata: {\"id\":\"job-1\",\"model\":\"first\"}\n\n"))
		case 2:
			if request.Header.Get("Last-Event-ID") != "snapshot-1" {
				t.Fatalf("reconnect Last-Event-ID=%q", request.Header.Get("Last-Event-ID"))
			}
			response.Body = io.NopCloser(strings.NewReader("event: snapshot\nid: snapshot-2\ndata: {\"id\":\"job-1\",\"model\":\"second\"}\n\n"))
		default:
			t.Fatalf("unexpected watch reconnect %d", requests)
		}
		return response, nil
	})
	client, err := New("https://dorf.example.test", credential, transport)
	if err != nil {
		t.Fatal(err)
	}
	var goals []string
	err = client.WatchSession(context.Background(), "job-1", func(session controlapi.Session) error {
		goals = append(goals, session.Model)
		if len(goals) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || requests != 2 || strings.Join(goals, ",") != "first,second" {
		t.Fatalf("Watch goals=%v requests=%d err=%v", goals, requests, err)
	}
}

func TestListSessionsEncodesOneOpaquePageRequest(t *testing.T) {
	const credential = "list-credential"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/sessions" ||
			request.URL.Query().Get("limit") != "2" || request.URL.Query().Get("cursor") != "opaque+/cursor" ||
			request.Header.Get("Authorization") != "Bearer "+credential {
			t.Fatalf("list request=%s %s auth=%q", request.Method, request.URL, request.Header.Get("Authorization"))
		}
		return jsonResponse(http.StatusOK, `{"sessions":[{"id":"job-2","admitted_at":"2026-08-26T12:00:00Z"}],"next_cursor":"next-page"}`), nil
	})
	client, err := New("https://dorf.example.test", credential, transport)
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.ListSessions(context.Background(), 2, "opaque+/cursor")
	if err != nil || len(page.Sessions) != 1 || page.Sessions[0].ID != "job-2" || page.NextCursor == nil || *page.NextCursor != "next-page" {
		t.Fatalf("Session page=%#v err=%v", page, err)
	}
	if _, err := client.ListSessions(context.Background(), 101, ""); err == nil {
		t.Fatal("out-of-range client limit was accepted")
	}
}

func TestSandboxFileReturnsExactVerifiedBytesAtReadLimit(t *testing.T) {
	const credential = "file-credential"
	contents := bytes.Repeat([]byte{0, 0xff, '\n', '\r'}, provider.MaxFileReadBytes/4)
	digest := sha256.Sum256(contents)
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/v1/sandboxes/sandbox-1/files" || request.URL.Query().Get("path") != "results/report #1.bin" || request.Header.Get("Authorization") != "Bearer "+credential || request.Header.Get("Accept-Encoding") != "identity" {
			t.Fatalf("file request = %s %s auth=%q encoding=%q", request.Method, request.URL, request.Header.Get("Authorization"), request.Header.Get("Accept-Encoding"))
		}
		body := contents
		if requests == 2 {
			body = []byte("tampered")
		}
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
		response.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
		return response, nil
	})
	client, err := New("https://dorf.example.test", credential, transport)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.SandboxFile(context.Background(), "sandbox-1", "results/report #1.bin")
	if err != nil || !bytes.Equal(got, contents) || len(got) != provider.MaxFileReadBytes {
		t.Fatalf("Sandbox file bytes=%d exact=%t err=%v", len(got), bytes.Equal(got, contents), err)
	}
	_, err = client.SandboxFile(context.Background(), "sandbox-1", "results/report #1.bin")
	if err == nil || strings.Contains(err.Error(), credential) {
		t.Fatalf("tampered Sandbox file err=%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
