package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRouteReconciliationIsStableAndRevocationIsIdempotent(t *testing.T) {
	var active [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": "codex-private-account@example.com.json", "provider": "codex", "websockets": true}}})
			return
		}
		if r.URL.Path != "/v0/management/api-keys" || r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer control-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var keys []string
		if err := json.NewDecoder(r.Body).Decode(&keys); err != nil {
			t.Error(err)
		}
		active = append(active, keys)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	state := gatewayState(t, server.URL)
	gateway := Gateway{StatePath: state, Client: server.Client()}

	first, err := gateway.ReconcileCreate(context.Background(), "primary", "sandbox:job-1", "action-stable")
	if err != nil {
		t.Fatal(err)
	}
	second, err := gateway.ReconcileCreate(context.Background(), "primary", "sandbox:job-1", "action-stable")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.APIKey != second.APIKey {
		t.Fatalf("route was duplicated: %#v %#v", first, second)
	}
	var routes []Route
	if err := readJSON(filepath.Join(state, "routes.json"), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 {
		t.Fatalf("routes=%d, want 1", len(routes))
	}
	if _, err := gateway.RevokeExact(context.Background(), "sandbox:job-1", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.RevokeExact(context.Background(), "sandbox:job-1", first.ID); err != nil {
		t.Fatal(err)
	}
	if got := active[len(active)-1]; len(got) != 1 || got[0] != "guard-secret" {
		t.Fatalf("active keys after revocation=%v", got)
	}
}

func TestExactRouteRevocationRefusesChangedIdentityAndReconcilesAbsence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": "codex-private-account@example.com.json", "provider": "codex", "websockets": true}}})
		case r.URL.Path == "/v0/management/api-keys" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	gateway := Gateway{StatePath: gatewayState(t, server.URL), Client: server.Client()}
	route, err := gateway.ReconcileCreate(context.Background(), "primary", "sandbox:job-exact", "action-exact")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.RevokeExact(context.Background(), "sandbox:job-exact", "route-foreign"); err == nil {
		t.Fatal("changed exact route identity was revoked")
	}
	if observed, present, err := gateway.Route(context.Background(), "sandbox:job-exact"); err != nil || !present || observed.ID != route.ID {
		t.Fatalf("route changed after fenced refusal: route=%#v present=%t err=%v", observed, present, err)
	}
	removed, err := gateway.RevokeExact(context.Background(), "sandbox:job-exact", route.ID)
	if err != nil || removed != route.ID {
		t.Fatalf("removed=%s err=%v", removed, err)
	}
	removed, err = gateway.RevokeExact(context.Background(), "sandbox:job-exact", route.ID)
	if err != nil || removed != "absent" {
		t.Fatalf("idempotent removed=%s err=%v", removed, err)
	}
	rebound, err := gateway.ReconcileCreate(context.Background(), "primary", "sandbox:job-exact", "action-rebound")
	if err != nil {
		t.Fatal(err)
	}
	if rebound.ID == route.ID {
		t.Fatal("rebound route did not receive its own stable Action identity")
	}
	if _, err := gateway.RevokeExact(context.Background(), "sandbox:job-exact", RouteID("action-exact")); err == nil {
		t.Fatal("stable prior Action identity revoked a rebound consumer route")
	}
	if observed, present, err := gateway.Route(context.Background(), "sandbox:job-exact"); err != nil || !present || observed.ID != rebound.ID {
		t.Fatalf("rebound route changed after fenced refusal: route=%#v present=%t err=%v", observed, present, err)
	}
}

func TestExactRouteRevocationReconcilesBrokerAfterActivationFailure(t *testing.T) {
	active := map[string]bool{}
	failActivation := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": "codex-private-account@example.com.json", "provider": "codex", "websockets": true}}})
		case r.URL.Path == "/v0/management/api-keys" && r.Method == http.MethodPut:
			var keys []string
			if err := json.NewDecoder(r.Body).Decode(&keys); err != nil {
				t.Error(err)
				http.Error(w, "invalid keys", http.StatusBadRequest)
				return
			}
			if failActivation {
				failActivation = false
				http.Error(w, "injected activation failure", http.StatusInternalServerError)
				return
			}
			active = map[string]bool{}
			for _, key := range keys {
				active[key] = true
			}
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !active[key] {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	state := gatewayState(t, server.URL)
	gateway := Gateway{StatePath: state, Client: server.Client()}
	accepted := func(key string) bool {
		request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+key)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK
	}

	target, err := gateway.ReconcileCreate(context.Background(), "primary", "sandbox:job-target", "action-target")
	if err != nil {
		t.Fatal(err)
	}
	sentinel, err := gateway.ReconcileCreate(context.Background(), "primary", "sandbox:job-sentinel", "action-sentinel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.RevokeExact(context.Background(), target.Consumer, "route-changed"); err == nil {
		t.Fatal("changed exact route identity was revoked")
	}
	if !accepted(target.APIKey) || !accepted(sentinel.APIKey) {
		t.Fatal("changed identity altered accepted keys")
	}

	failActivation = true
	if _, err := gateway.RevokeExact(context.Background(), target.Consumer, target.ID); err == nil {
		t.Fatal("injected activation failure was not returned")
	}
	var routes []Route
	if err := readJSON(filepath.Join(state, "routes.json"), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].ID != sentinel.ID {
		t.Fatalf("durable routes after failed activation=%#v, want only sentinel", routes)
	}
	if !accepted(target.APIKey) || !accepted(sentinel.APIKey) {
		t.Fatal("failed activation unexpectedly changed accepted keys")
	}

	removed, err := gateway.RevokeExact(context.Background(), target.Consumer, target.ID)
	if err != nil || removed != "absent" {
		t.Fatalf("retry removed=%q err=%v", removed, err)
	}
	if accepted(target.APIKey) {
		t.Fatal("revoked route key remained active after absent-route reconciliation")
	}
	if !accepted(sentinel.APIKey) {
		t.Fatal("sentinel route key was removed during target reconciliation")
	}
}

func TestRouteFailsClosedWhenChatGPTWebSocketsAreNotVerified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"name": "codex-private-account@example.com.json", "provider": "codex", "websockets": false}}})
		case r.URL.Path == "/v0/management/api-keys" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	gateway := Gateway{StatePath: gatewayState(t, server.URL), Client: server.Client()}

	if err := gateway.Check(context.Background(), "primary"); err == nil {
		t.Fatal("provider readiness accepted unverified upstream WebSockets")
	}
	if _, err := gateway.ReconcileCreate(context.Background(), "primary", "sandbox:job-http", "action-http"); err == nil {
		t.Fatal("route was admitted without verified upstream WebSockets")
	}
	var routes []Route
	if err := readJSON(filepath.Join(gateway.StatePath, "routes.json"), &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes=%d, want none after capability rejection", len(routes))
	}
}

func TestAPIRoutesDoNotRequireChatGPTSubscriptionCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files" {
			t.Error("API-key route queried ChatGPT OAuth capability")
			http.Error(w, "unexpected capability query", http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/v0/management/api-keys" && r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	for _, provider := range []string{"openai", "deepseek"} {
		t.Run(provider, func(t *testing.T) {
			state := gatewayState(t, server.URL)
			if err := os.Mkdir(filepath.Join(state, "credentials"), 0o700); err != nil {
				t.Fatal(err)
			}
			credentialRef := provider + "-0123456789abcdef.key"
			if err := os.WriteFile(filepath.Join(state, "credentials", credentialRef), []byte("test-secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			connections, _ := json.Marshal([]connection{{Name: provider, Provider: provider, AuthMode: "api_key", CredentialRef: credentialRef}})
			if err := os.WriteFile(filepath.Join(state, "connections.json"), connections, 0o600); err != nil {
				t.Fatal(err)
			}

			gateway := Gateway{StatePath: state, Client: server.Client()}
			if _, err := gateway.ReconcileCreate(context.Background(), provider, "sandbox:job-"+provider, "action-"+provider); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func gatewayState(t *testing.T, origin string) string {
	t.Helper()
	state := t.TempDir()
	parsed, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(parsed.Port(), "%d", &port); err != nil {
		t.Fatal(err)
	}
	mustWrite := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(state, path), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(state, "auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite("auth/codex-private-account@example.com.json", "{}")
	mustWrite("connections.json", `[{"name":"primary","provider":"chatgpt","auth_mode":"subscription","credential_ref":"codex-private-account@example.com.json"}]`)
	mustWrite("authority.json", `{"guard_key":"guard-secret","management_key":"control-secret"}`)
	mustWrite("routes.json", "[]")
	mustWrite("broker.yaml", fmt.Sprintf("host: %q\nport: %d\n", parsed.Hostname(), port))
	return state
}
