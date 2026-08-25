package controlclient

import (
	"context"
	"encoding/json"
	"errors"
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
