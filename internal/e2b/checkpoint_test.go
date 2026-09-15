package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type checkpointResponseLoss struct {
	base http.RoundTripper
	path string
}

func (r *checkpointResponseLoss) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	if err == nil && request.Method == http.MethodPost && request.URL.Path == r.path && response.StatusCode == http.StatusCreated {
		r.path = ""
		response.Body.Close()
		return nil, errors.New("accepted checkpoint operation response lost")
	}
	return response, err
}

func TestCheckpointAndReplacementRecoverLostResponsesWithoutCrossingOwners(t *testing.T) {
	ctx := context.Background()
	source := provider.Ownership{JobID: "job", SandboxID: "logical", OwnershipNonce: strings.Repeat("a", 64)}
	destination := source
	destination.OwnershipNonce = strings.Repeat("b", 64)
	api := newFakeAPI(t)
	api.sandboxes["source"] = detailSandbox{SandboxID: "source", State: "running", Metadata: e2bOwnership(source).metadata()}
	name := ""
	present := false
	captures, creates, deletes := 0, 0, 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots":
			items := []savedSnapshot{}
			if present && r.URL.Query().Get("sandboxID") == "source" && (r.URL.Query().Get("name") == name || r.URL.Query().Get("name") == "snapshot:fixed") {
				items = append(items, savedSnapshot{ID: "snapshot:fixed"})
			}
			json.NewEncoder(w).Encode(items)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/source/snapshots":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			name, present = body["name"], true
			captures++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(savedSnapshot{ID: "snapshot:fixed"})
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/source/pause":
			old := api.sandboxes["source"]
			old.State = "paused"
			api.sandboxes["source"] = old
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/templates/snapshot:fixed":
			deletes++
			present = false
			w.WriteHeader(http.StatusNoContent)
		default:
			if r.Method == http.MethodPost && r.URL.Path == "/sandboxes" {
				creates++
			}
			api.ServeHTTP(w, r)
		}
	})
	loss := &checkpointResponseLoss{base: handlerTransport{handler: handler}, path: "/sandboxes/source/snapshots"}
	adapter := Adapter{Client: Client{APIURL: "https://e2b.test", APIKey: "test-key", HTTPClient: &http.Client{Transport: loss}}, Config: AdapterConfig{SandboxTimeout: 10 * time.Minute, AllowInternet: true}}
	if _, err := adapter.CaptureCheckpoint(ctx, source, "upgrade"); err == nil {
		t.Fatal("lost checkpoint acknowledgement was hidden")
	}
	checkpoint, err := adapter.CaptureCheckpoint(ctx, source, "upgrade")
	if err != nil || captures != 1 {
		t.Fatalf("capture retry duplicated snapshot: count=%d err=%v", captures, err)
	}
	if _, err := adapter.RestoreCheckpoint(ctx, source, source, checkpoint); err == nil {
		t.Fatal("replacement reused source ownership")
	}
	foreign := source
	foreign.OwnershipNonce = strings.Repeat("c", 64)
	if _, err := adapter.RestoreCheckpoint(ctx, foreign, destination, checkpoint); err == nil {
		t.Fatal("foreign source was accepted")
	}
	if err := adapter.DeleteCheckpoint(ctx, foreign, checkpoint); err == nil {
		t.Fatal("foreign checkpoint deletion was reported successful")
	}
	if deletes != 0 {
		t.Fatal("foreign checkpoint was deleted")
	}
	loss.path = "/sandboxes"
	if _, err := adapter.RestoreCheckpoint(ctx, source, destination, checkpoint); err == nil {
		t.Fatal("lost replacement acknowledgement was hidden")
	}
	replacement, err := adapter.RestoreCheckpoint(ctx, source, destination, checkpoint)
	if err != nil || replacement != "provider-1" || creates != 1 {
		t.Fatalf("replacement retry=%q creates=%d err=%v", replacement, creates, err)
	}
	old, err := adapter.Client.FindOwned(ctx, e2bOwnership(source))
	if err != nil || old == nil || old.ProviderID != "source" {
		t.Fatal("replacement obscured original cleanup identity")
	}
	if api.createBody["templateID"] != checkpoint.Reference {
		t.Fatal("replacement used the profile image instead of checkpoint")
	}
	if err := adapter.DeleteCheckpoint(ctx, source, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DeleteCheckpoint(ctx, source, checkpoint); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 {
		t.Fatal("checkpoint cleanup was not idempotent")
	}
}
