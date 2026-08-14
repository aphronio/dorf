package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLifecycleReconcilesLostCreateResponseAndDeletesOnlyOwnedSandbox(t *testing.T) {
	api := newFakeAPI(t)

	owner := Ownership{JobID: "job-1", SandboxID: "dorf-job-1", OwnershipNonce: strings.Repeat("a", 64)}
	client := Client{
		APIURL: "https://e2b.test",
		APIKey: "test-key",
		HTTPClient: &http.Client{Transport: &dropCreatedResponse{
			base: handlerTransport{handler: api},
		}},
	}
	_, err := client.Create(context.Background(), CreateRequest{Template: "template:build", Timeout: 10 * time.Minute, Owner: owner})
	if err == nil || !strings.Contains(err.Error(), "injected lost response") {
		t.Fatalf("ambiguous create error = %v", err)
	}

	discovered, err := client.FindOwned(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if discovered == nil || discovered.ProviderID != "provider-1" {
		t.Fatalf("discovered Sandbox = %#v", discovered)
	}
	inspected, err := client.InspectOwned(context.Background(), discovered.ProviderID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.TemplateID != "template:build" || inspected.State != "running" {
		t.Fatalf("inspected Sandbox = %#v", inspected)
	}

	foreign := owner
	foreign.OwnershipNonce = strings.Repeat("b", 64)
	if err := client.DeleteOwned(context.Background(), discovered.ProviderID, foreign); err == nil {
		t.Fatal("foreign ownership was allowed to delete the Sandbox")
	}
	if api.deleteCalls != 0 {
		t.Fatalf("foreign ownership made %d delete calls", api.deleteCalls)
	}
	if err := client.DeleteOwned(context.Background(), discovered.ProviderID, owner); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteOwned(context.Background(), discovered.ProviderID, owner); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	api.mu.Lock()
	api.sandboxes["provider-2"] = detailSandbox{SandboxID: "provider-2", State: "running", Metadata: owner.metadata()}
	api.mu.Unlock()
	if err := client.DeleteOwned(context.Background(), discovered.ProviderID, owner); err == nil || !strings.Contains(err.Error(), "owned resource provider-2 remains") {
		t.Fatalf("stale provider locator error = %v", err)
	}
	if api.deleteCalls != 1 {
		t.Fatalf("stale provider locator made %d delete calls, want 1", api.deleteCalls)
	}
	api.mu.Lock()
	delete(api.sandboxes, "provider-2")
	api.mu.Unlock()
	absent, err := client.FindOwned(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if absent != nil {
		t.Fatalf("deleted Sandbox remains discoverable: %#v", absent)
	}

	if api.createBody["templateID"] != "template:build" || api.createBody["secure"] != true || api.createBody["allow_internet_access"] != false {
		t.Fatalf("create body = %#v", api.createBody)
	}
	if api.createBody["timeout"] != float64(600) {
		t.Fatalf("create timeout = %#v", api.createBody["timeout"])
	}
	if api.authorizationFailures != 0 {
		t.Fatalf("requests missing exact API authentication = %d", api.authorizationFailures)
	}
}

func TestFindOwnedPaginatesRunningAndPausedAndRejectsDuplicates(t *testing.T) {
	owner := Ownership{JobID: "job-2", SandboxID: "dorf-job-2", OwnershipNonce: strings.Repeat("c", 64)}
	requests := 0
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v2/sandboxes" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if state := request.URL.Query().Get("state"); state != "running,paused" {
			t.Fatalf("state = %q", state)
		}
		inner, err := url.ParseQuery(request.URL.Query().Get("metadata"))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range owner.metadata() {
			if inner.Get(key) != value {
				t.Fatalf("metadata[%q] = %q", key, inner.Get(key))
			}
		}
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("nextToken") == "" {
			response.Header().Set("X-Next-Token", "page-2")
			json.NewEncoder(response).Encode([]listedSandbox{{SandboxID: "foreign", Metadata: map[string]string{metadataOwner: "foreign"}}})
			return
		}
		json.NewEncoder(response).Encode([]listedSandbox{
			{SandboxID: "provider-1", State: "running", Metadata: owner.metadata()},
			{SandboxID: "provider-2", State: "paused", Metadata: owner.metadata()},
		})
	})

	client := Client{APIURL: "https://e2b.test", APIKey: "test-key", HTTPClient: &http.Client{Transport: handlerTransport{handler: handler}}}
	_, err := client.FindOwned(context.Background(), owner)
	var ownershipErr *OwnershipError
	if !errors.As(err, &ownershipErr) || !strings.Contains(err.Error(), "2 exact matches") {
		t.Fatalf("duplicate ownership error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("list requests = %d, want 2", requests)
	}
}

func TestAPIErrorPreservesProviderStatusWithoutLeakingKey(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(response, `{"code":429,"message":"capacity reached"}`)
	})
	client := Client{APIURL: "https://e2b.test", APIKey: "secret-test-key", HTTPClient: &http.Client{Transport: handlerTransport{handler: handler}}}
	owner := Ownership{JobID: "job-3", SandboxID: "dorf-job-3", OwnershipNonce: strings.Repeat("d", 64)}
	_, err := client.FindOwned(context.Background(), owner)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != 429 {
		t.Fatalf("API error = %#v", err)
	}
	if strings.Contains(err.Error(), client.APIKey) {
		t.Fatal("API error leaked the E2B key")
	}
}

type dropCreatedResponse struct {
	base http.RoundTripper
	once sync.Once
}

type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func (t *dropCreatedResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	drop := false
	if request.Method == http.MethodPost && request.URL.Path == "/sandboxes" && response.StatusCode == http.StatusCreated {
		t.once.Do(func() { drop = true })
	}
	if !drop {
		return response, nil
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return nil, errors.New("injected lost response after E2B accepted create")
}

type fakeAPI struct {
	t                     *testing.T
	mu                    sync.Mutex
	sandboxes             map[string]detailSandbox
	createBody            map[string]any
	deleteCalls           int
	authorizationFailures int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{t: t, sandboxes: map[string]detailSandbox{}}
}

func (a *fakeAPI) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if request.Header.Get("X-API-Key") != "test-key" {
		a.authorizationFailures++
		response.WriteHeader(http.StatusUnauthorized)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/sandboxes":
		if err := json.NewDecoder(request.Body).Decode(&a.createBody); err != nil {
			a.t.Fatal(err)
		}
		metadata := map[string]string{}
		for key, value := range a.createBody["metadata"].(map[string]any) {
			metadata[key] = value.(string)
		}
		now := time.Now().UTC()
		a.sandboxes["provider-1"] = detailSandbox{
			SandboxID: "provider-1", TemplateID: a.createBody["templateID"].(string), EnvdVersion: "0.6.2",
			State: "running", Metadata: metadata, StartedAt: now, EndAt: now.Add(10 * time.Minute),
		}
		response.WriteHeader(http.StatusCreated)
		json.NewEncoder(response).Encode(map[string]any{
			"sandboxID": "provider-1", "templateID": a.createBody["templateID"], "envdVersion": "0.6.2",
			"envdAccessToken": "must-not-be-retained", "trafficAccessToken": "must-not-be-retained",
		})
	case request.Method == http.MethodGet && request.URL.Path == "/v2/sandboxes":
		inner, err := url.ParseQuery(request.URL.Query().Get("metadata"))
		if err != nil {
			a.t.Fatal(err)
		}
		listed := []listedSandbox{}
		for _, sandbox := range a.sandboxes {
			match := true
			for key, values := range inner {
				if len(values) != 1 || sandbox.Metadata[key] != values[0] {
					match = false
				}
			}
			if match {
				listed = append(listed, listedSandbox(sandbox))
			}
		}
		json.NewEncoder(response).Encode(listed)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/sandboxes/"):
		id := strings.TrimPrefix(request.URL.Path, "/sandboxes/")
		sandbox, ok := a.sandboxes[id]
		if !ok {
			response.WriteHeader(http.StatusNotFound)
			json.NewEncoder(response).Encode(map[string]any{"code": 404, "message": "not found"})
			return
		}
		json.NewEncoder(response).Encode(sandbox)
	case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/sandboxes/"):
		a.deleteCalls++
		id := strings.TrimPrefix(request.URL.Path, "/sandboxes/")
		if _, ok := a.sandboxes[id]; !ok {
			response.WriteHeader(http.StatusNotFound)
			json.NewEncoder(response).Encode(map[string]any{"code": 404, "message": "not found"})
			return
		}
		delete(a.sandboxes, id)
		response.WriteHeader(http.StatusNoContent)
	default:
		a.t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
	}
}
