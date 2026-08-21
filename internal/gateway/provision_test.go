package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestProviderBindIsLoopbackOrExactPrivateIncusBridge(t *testing.T) {
	bridge := []net.Addr{&net.IPNet{IP: net.ParseIP("10.44.0.1"), Mask: net.CIDRMask(24, 32)}}
	tests := []struct {
		name       string
		bind       string
		addresses  []net.Addr
		want       string
		wantRemote bool
		wantError  string
	}{
		{name: "loopback", bind: "127.0.0.2", want: "127.0.0.2"},
		{name: "private Incus bridge", bind: "10.44.0.1", addresses: bridge, want: "10.44.0.1", wantRemote: true},
		{name: "private LAN", bind: "192.168.1.20", addresses: bridge, wantError: "not assigned"},
		{name: "public interface", bind: "203.0.113.10", addresses: bridge, wantError: "public interface"},
		{name: "wildcard", bind: "0.0.0.0", wantError: "loopback or"},
		{name: "link local", bind: "169.254.1.1", wantError: "loopback or"},
		{name: "IPv6", bind: "::1", wantError: "loopback or"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, remote, err := validateProviderBind(test.bind, test.addresses)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error=%v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil || got != test.want || remote != test.wantRemote {
				t.Fatalf("bind=%q remote=%t error=%v", got, remote, err)
			}
		})
	}
}

func TestOpenAIAPIKeyConnectionIsPrivateIdempotentAndRotatable(t *testing.T) {
	g := Gateway{StatePath: t.TempDir()}
	if err := g.ensureAuthority(); err != nil {
		t.Fatal(err)
	}
	if err := g.ensureStateFiles(); err != nil {
		t.Fatal(err)
	}
	created, err := g.recordOpenAIAPIKey("openai-api", "sk-test-secret")
	if err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	replayed, err := g.recordOpenAIAPIKey("openai-api", "sk-test-secret")
	if err != nil || replayed {
		t.Fatalf("replayed=%t err=%v", replayed, err)
	}
	rotated, err := g.recordOpenAIAPIKey("openai-api", "sk-another-secret")
	if err != nil || !rotated {
		t.Fatalf("rotated=%t err=%v", rotated, err)
	}
	if _, err := g.recordOpenAIAPIKey("second-openai", "sk-second"); err == nil || !strings.Contains(err.Error(), "one upstream authentication mode") {
		t.Fatalf("ambiguous upstream error=%v", err)
	}
	records, err := g.connections()
	if err != nil || len(records) != 1 {
		t.Fatalf("connections=%v err=%v", records, err)
	}
	credential := filepath.Join(g.StatePath, "credentials", records[0].CredentialRef)
	info, err := os.Stat(credential)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode=%o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Join(g.StatePath, "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-test-secret") || strings.Contains(string(raw), "sk-another-secret") {
		t.Fatal("connection metadata contains the OpenAI API key")
	}
	secret, err := os.ReadFile(credential)
	if err != nil || strings.TrimSpace(string(secret)) != "sk-another-secret" {
		t.Fatalf("rotated credential=%q err=%v", strings.TrimSpace(string(secret)), err)
	}
}

func TestOpenAIAPIKeyReadinessUsesOfficialAuthenticatedEndpoint(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		wantError  string
	}{
		{name: "accepted", statusCode: http.StatusOK},
		{name: "rejected", statusCode: http.StatusUnauthorized, wantError: "HTTP 401"},
		{name: "not ready", statusCode: http.StatusTooManyRequests, wantError: "HTTP 429"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodGet || request.URL.String() != openAIModelsURL {
					t.Fatalf("request=%s %s", request.Method, request.URL)
				}
				if got := request.Header.Get("Authorization"); got != "Bearer sk-private" {
					t.Fatalf("authorization=%q", got)
				}
				return &http.Response{
					StatusCode: test.statusCode,
					Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"must not leak"}}`)),
					Header:     make(http.Header),
				}, nil
			})}
			err := (Gateway{UpstreamClient: client}).validateOpenAIAPIKey(context.Background(), "sk-private")
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("error=%v, want %q", err, test.wantError)
			}
			if err != nil && strings.Contains(err.Error(), "must not leak") {
				t.Fatal("readiness error leaked the provider response body")
			}
		})
	}
}
