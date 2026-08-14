package sandbox

import (
	"net/http"
	"strings"
	"testing"
)

func TestEndpointClonesAndRedactsProviderCapabilities(t *testing.T) {
	original := make(http.Header)
	original.Set("e2b-traffic-access-token", "private-capability")
	endpoint := NewEndpoint("ws://0.0.0.0:4500", "wss://4500-sandbox.example", original)
	original.Set("e2b-traffic-access-token", "mutated")
	first := endpoint.Headers()
	if first.Get("e2b-traffic-access-token") != "private-capability" {
		t.Fatalf("endpoint headers = %#v", first)
	}
	first.Set("Authorization", "foreign")
	if endpoint.Headers().Get("Authorization") != "" {
		t.Fatal("caller mutated provider-owned endpoint headers")
	}
	if strings.Contains(endpoint.String(), "private-capability") || strings.Contains(endpoint.GoString(), "private-capability") {
		t.Fatal("endpoint formatting leaked its provider capability")
	}
}
