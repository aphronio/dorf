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
	"net/http"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
)

func TestProblemsRedirectsAndOversizedResponsesDoNotLeakCredential(t *testing.T) {
	const credential = "never-print-this-credential"
	escapedGoal := strings.Repeat("\x00", 1<<20)
	escapedJob, err := json.Marshal(controlapi.Job{ID: "job-1", Goal: escapedGoal})
	if err != nil {
		t.Fatal(err)
	}
	if len(escapedJob) <= 2<<20 || len(escapedJob) > maxResponseBytes {
		t.Fatalf("escaped 1 MiB goal response size=%d", len(escapedJob))
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
			return jsonResponse(http.StatusOK, string(escapedJob)), nil
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
	if !errors.As(err, &problem) || problem.Problem.Code != "invalid_client" || strings.Contains(err.Error(), credential) {
		t.Fatalf("problem=%#v err=%v", problem, err)
	}
	job, err := client.Job(context.Background(), "job-1")
	if err != nil || job.Goal != escapedGoal {
		t.Fatalf("escaped Job goal length=%d err=%v", len(job.Goal), err)
	}
	if _, err := client.Me(context.Background()); err == nil || strings.Contains(err.Error(), credential) {
		t.Fatalf("oversized response err=%v", err)
	}
	if _, err := client.Me(context.Background()); err == nil || strings.Contains(err.Error(), credential) {
		t.Fatalf("transport error err=%v", err)
	}
}

func TestWatchJobReconnectsWithoutOrdinaryRequestTimeout(t *testing.T) {
	const credential = "watch-credential"
	stop := errors.New("snapshots complete")
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/v1/jobs/job-1/watch" || request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("Authorization") != "Bearer "+credential {
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
			response.Body = io.NopCloser(strings.NewReader(": connected\nretry: 0\nevent: snapshot\nid: snapshot-1\ndata: {\"id\":\"job-1\",\"goal\":\"first\"}\n\n"))
		case 2:
			if request.Header.Get("Last-Event-ID") != "snapshot-1" {
				t.Fatalf("reconnect Last-Event-ID=%q", request.Header.Get("Last-Event-ID"))
			}
			response.Body = io.NopCloser(strings.NewReader("event: snapshot\nid: snapshot-2\ndata: {\"id\":\"job-1\",\"goal\":\"second\"}\n\n"))
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
	err = client.WatchJob(context.Background(), "job-1", func(job controlapi.Job) error {
		goals = append(goals, job.Goal)
		if len(goals) == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || requests != 2 || strings.Join(goals, ",") != "first,second" {
		t.Fatalf("Watch goals=%v requests=%d err=%v", goals, requests, err)
	}
}

func TestSandboxFileReturnsExactVerifiedBytesWithoutJSONLimit(t *testing.T) {
	const credential = "file-credential"
	contents := append(bytes.Repeat([]byte{0, 0xff, '\n', '\r'}, maxResponseBytes/4+1), []byte("exact tail")...)
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
	if err != nil || !bytes.Equal(got, contents) || len(got) <= maxResponseBytes {
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
