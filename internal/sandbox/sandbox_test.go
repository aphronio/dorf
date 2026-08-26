package sandbox

import (
	"context"
	"net"
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

func TestEndpointCarriesAProviderDialCapabilityWithoutFormattingIt(t *testing.T) {
	wantClient, wantServer := net.Pipe()
	t.Cleanup(func() { _ = wantClient.Close() })
	t.Cleanup(func() { _ = wantServer.Close() })

	calls := 0
	dial := func(_ context.Context, network, address string) (net.Conn, error) {
		calls++
		if network != "tcp" || address != "incus.invalid:4500" {
			t.Fatalf("dial target = %s %s", network, address)
		}
		return wantClient, nil
	}
	endpoint := NewDialEndpoint("ws://127.0.0.1:4500", "ws://incus.invalid:4500", nil, dial)

	got, err := endpoint.DialContext()(context.Background(), "tcp", "incus.invalid:4500")
	if err != nil {
		t.Fatal(err)
	}
	if got != wantClient || calls != 1 {
		t.Fatalf("provider dial result=%p calls=%d", got, calls)
	}
	if strings.Contains(endpoint.String(), "DialContext") || strings.Contains(endpoint.GoString(), "DialContext") {
		t.Fatal("endpoint formatting disclosed its provider dial capability")
	}
}
