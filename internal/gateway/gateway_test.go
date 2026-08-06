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
	"testing"
)

func TestRouteReconciliationIsStableAndRevocationIsIdempotent(t *testing.T) {
	var active [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if _, err := gateway.Revoke(context.Background(), "sandbox:job-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Revoke(context.Background(), "sandbox:job-1"); err != nil {
		t.Fatal(err)
	}
	if got := active[len(active)-1]; len(got) != 1 || got[0] != "guard-secret" {
		t.Fatalf("active keys after revocation=%v", got)
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
	mustWrite("auth/codex-dorf-0123456789abcdef.json", "{}")
	mustWrite("connections.json", `[{"name":"primary","provider":"chatgpt","auth_mode":"subscription","credential_ref":"codex-dorf-0123456789abcdef.json"}]`)
	mustWrite("authority.json", `{"guard_key":"guard-secret","management_key":"control-secret"}`)
	mustWrite("routes.json", "[]")
	mustWrite("broker.yaml", fmt.Sprintf("host: %q\nport: %d\n", parsed.Hostname(), port))
	return state
}
