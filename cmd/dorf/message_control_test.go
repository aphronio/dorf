package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/controlclient"
)

func TestMessageCLIDefaultAndExactStop(t *testing.T) {
	requests := make(chan string, 2)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-credential" {
			t.Error("missing authentication")
		}
		requests <- r.Method + " " + r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		message := controlapi.Message{JobID: "job", ID: "message", Intent: "steer", Delivery: controlapi.State{State: "running"}}
		switch r.Method {
		case http.MethodPost:
			var input controlapi.SendMessageRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Intent != "auto" || input.Text != "correction" || r.Header.Get("Idempotency-Key") != "send-key" {
				t.Errorf("default message request=%+v err=%v", input, err)
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodPut:
			message.InterruptRequested = true
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
		_ = json.NewEncoder(w).Encode(message)
	}))
	defer server.Close()
	client, err := controlclient.New(server.URL, "test-credential", server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "message.txt")
	if err := os.WriteFile(input, []byte("correction"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := clientconfig.Config{DeploymentURL: server.URL}
	var output, diagnostic bytes.Buffer
	if err := remoteMessageSend(context.Background(), cfg, client, []string{"--key", "send-key", "--input-file", input, "--output", "json", "job"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := remoteMessageInterrupt(context.Background(), cfg, client, []string{"--output", "json", "job", "message"}, &output, &diagnostic); err != nil {
		t.Fatal(err)
	}
	var receipt remoteMessageReceipt
	if err := json.Unmarshal(output.Bytes(), &receipt); err != nil || !receipt.Message.InterruptRequested {
		t.Fatalf("Stop receipt=%+v err=%v", receipt, err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count=%d", len(requests))
	}
	if first, second := <-requests, <-requests; first != "POST /v1/jobs/job/messages" || second != "PUT /v1/jobs/job/messages/message/interrupt" {
		t.Fatalf("requests=%s, %s", first, second)
	}
}
