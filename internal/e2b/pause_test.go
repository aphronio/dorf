package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMemoryPauseReconcilesLostAcknowledgementAndReconnect(t *testing.T) {
	api := newFakeAPI(t)
	owner := Ownership{SessionID: "pause-session", SandboxID: "pause-sandbox", OwnershipNonce: strings.Repeat("a", 64)}
	state := "running"
	pauses := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pause") {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["memory"] != true || len(body) != 1 {
				t.Fatalf("pause body=%v err=%v", body, err)
			}
			pauses++
			state = "paused"
			api.mu.Lock()
			item := api.sandboxes["provider-1"]
			item.State = state
			api.sandboxes["provider-1"] = item
			api.mu.Unlock()
			// Provider accepted the pause but the client cannot know that yet.
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/connect") {
			state = "running"
		}
		api.ServeHTTP(w, r)
	})
	client := Client{APIURL: "https://e2b.test", APIKey: "test-key", HTTPClient: &http.Client{Transport: handlerTransport{handler: handler}}}
	if _, err := client.Create(context.Background(), CreateRequest{Template: "template:build", Timeout: 10 * time.Minute, Owner: owner, AllowInternet: true}); err != nil {
		t.Fatal(err)
	}
	if api.createBody["autoPause"] != true {
		t.Fatal("expiry must preserve the sandbox")
	}
	foreign := owner
	foreign.OwnershipNonce = strings.Repeat("b", 64)
	if err := client.PauseOwned(context.Background(), "provider-1", foreign); err == nil || pauses != 0 {
		t.Fatal("foreign pause admitted")
	}
	if err := client.PauseOwned(context.Background(), "provider-1", owner); err == nil {
		t.Fatal("lost acknowledgement must be uncertain")
	}
	if err := client.PauseOwned(context.Background(), "provider-1", owner); err != nil || pauses != 1 {
		t.Fatalf("retry duplicated pause: %d %v", pauses, err)
	}
	if _, err := client.ConnectEnvd(context.Background(), "provider-1", 10*time.Minute); err != nil || state != "running" {
		t.Fatalf("resume failed %v", err)
	}
}

func TestPauseRefusalPreservesRetryableProviderError(t *testing.T) {
	owner := Ownership{SessionID: "session", SandboxID: "sandbox", OwnershipNonce: strings.Repeat("a", 64)}
	client := Client{APIURL: "https://e2b.test", APIKey: "test-key", HTTPClient: &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(detailSandbox{SandboxID: "provider", State: "running", Metadata: owner.metadata()})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})}}}
	var apiErr *APIError
	if err := client.PauseOwned(context.Background(), "provider", owner); !errors.As(err, &apiErr) || apiErr.StatusCode != 503 {
		t.Fatalf("refusal=%v", err)
	}
}

func TestPauseAcceptsConcurrentProviderAutoPause(t *testing.T) {
	owner := Ownership{SessionID: "session", SandboxID: "sandbox", OwnershipNonce: strings.Repeat("a", 64)}
	client := Client{APIURL: "https://e2b.test", APIKey: "test-key", HTTPClient: &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(detailSandbox{SandboxID: "provider", State: "running", Metadata: owner.metadata()})
			return
		}
		w.WriteHeader(http.StatusConflict)
	})}}}
	if err := client.PauseOwned(context.Background(), "provider", owner); err != nil {
		t.Fatalf("already paused: %v", err)
	}
}
