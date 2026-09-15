package controlclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/aphronio/dorf/internal/controlapi"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestSandboxFileClientEnforcesBoundBeforeLengthAndDigest(t *testing.T) {
	tests := []struct {
		name          string
		body          []byte
		contentLength int64
		digest        string
		wantTooLarge  bool
		wantErr       bool
	}{
		{name: "exact limit", body: bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes), contentLength: provider.MaxFileReadBytes},
		{name: "missing length plus one", body: bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes+1), contentLength: -1, wantTooLarge: true, wantErr: true},
		{name: "lying short length plus one", body: bytes.Repeat([]byte{'x'}, provider.MaxFileReadBytes+1), contentLength: provider.MaxFileReadBytes - 1, wantTooLarge: true, wantErr: true},
		{name: "advertised oversize", body: []byte("unused"), contentLength: provider.MaxFileReadBytes + 1, wantTooLarge: true, wantErr: true},
		{name: "conflicting short body", body: []byte("short"), contentLength: 6, wantErr: true},
		{name: "malformed digest", body: []byte("short"), contentLength: 5, digest: "sha-256=:not-base64:", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			digest := test.digest
			if digest == "" {
				sum := sha256.Sum256(test.body)
				digest = "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
			}
			client, err := New("https://dorf.example.test", "credential", fileClientRoundTripFunc(func(*http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(test.body)), ContentLength: test.contentLength}
				response.Header.Set("Content-Digest", digest)
				return response, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			contents, err := client.SandboxFile(context.Background(), "sandbox-1", "result.bin")
			if (err != nil) != test.wantErr || errors.Is(err, controlapi.ErrFileTooLarge) != test.wantTooLarge {
				t.Fatalf("SandboxFile() bytes=%d err=%v, want error=%t too_large=%t", len(contents), err, test.wantErr, test.wantTooLarge)
			}
			if err == nil && !bytes.Equal(contents, test.body) {
				t.Fatal("SandboxFile() did not preserve exact-limit bytes")
			}
		})
	}
}

func TestSandboxFileProblemRetainsWireAndTypedClassification(t *testing.T) {
	client, err := New("https://dorf.example.test", "credential", fileClientRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusConflict,
			Header:     http.Header{"Content-Type": []string{"application/problem+json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"type":"https://dorf.dev/problems/file-too-large","title":"Sandbox file exceeds the read limit","status":409,"code":"file_too_large","retryable":false,"details":{}}`)),
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.SandboxFile(context.Background(), "sandbox-1", "result.bin")
	var problem *ProblemError
	if !errors.Is(err, controlapi.ErrFileTooLarge) || !errors.As(err, &problem) || problem.Problem.Code != "file_too_large" || !IsServiceError(err) {
		t.Fatalf("SandboxFile() err=%v problem=%#v service=%t", err, problem, IsServiceError(err))
	}
}

type fileClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f fileClientRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
