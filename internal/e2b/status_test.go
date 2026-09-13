package e2b

import (
	"context"
	"encoding/json"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"net/http"
	"strings"
	"testing"
)

func TestStatusOnlyReadsProviderMetadata(t *testing.T) {
	owner := provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: strings.Repeat("a", 64)}
	for _, state := range []string{"running", "paused", "future-state", "missing"} {
		t.Run(state, func(t *testing.T) {
			client := Client{APIURL: "https://e2b.test", APIKey: "key", HTTPClient: &http.Client{Transport: handlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v2/sandboxes" || r.URL.Query().Get("state") != "running,paused" {
					t.Fatalf("observation attempted non-list request: %s %s", r.Method, r.URL.Path)
				}
				items := []listedSandbox{}
				if state != "missing" {
					items = append(items, listedSandbox{SandboxID: "provider-1", State: state, Metadata: e2bOwnership(owner).metadata()})
				}
				json.NewEncoder(w).Encode(items)
			})}}}
			result, err := (Adapter{Client: client}).ObserveOwned(context.Background(), owner)
			want := state
			if state == "future-state" {
				want = "unknown"
			}
			if err != nil || result.Provider != "e2b" || result.State != want {
				t.Fatalf("status=%+v err=%v", result, err)
			}
		})
	}
}
