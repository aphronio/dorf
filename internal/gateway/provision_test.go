package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestPrepareCreatesPinnedBrokerConfigurationWithoutStartingIt(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)

	if err := g.Prepare(context.Background(), "127.0.0.2"); err != nil {
		t.Fatal(err)
	}

	config, err := os.ReadFile(filepath.Join(state, "broker.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), `host: "127.0.0.2"`) || !strings.Contains(string(config), "port: 8317") {
		t.Fatalf("broker configuration=%q", config)
	}
	if _, err := os.Stat(filepath.Join(state, "authority.json")); err != nil {
		t.Fatalf("provider authority was not prepared: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "broker.pid")); !os.IsNotExist(err) {
		t.Fatalf("preparation started a broker or wrote its PID: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "foreground.started")); !os.IsNotExist(err) {
		t.Fatalf("preparation ran the broker: %v", err)
	}
}

func TestOnlyContainerPreparationAllowsWildcardBrokerBind(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)

	if err := g.Prepare(context.Background(), "0.0.0.0"); err == nil {
		t.Fatalf("ordinary wildcard preparation error=%v", err)
	}
	if err := g.PrepareContainer(context.Background(), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	config, err := os.ReadFile(filepath.Join(state, "broker.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), `host: "0.0.0.0"`) || !strings.Contains(string(config), "  allow-remote: true") {
		t.Fatalf("container broker configuration=%q", config)
	}
	origin, err := g.internalDialOrigin()
	if err != nil || origin != "http://127.0.0.1:8317" {
		t.Fatalf("host dial origin=%q err=%v", origin, err)
	}
}

func TestComposeStateRequiresOnePreparedConnectionAndTracksPublication(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)
	if err := g.PrepareContainer(context.Background(), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, desired, err := g.ComposeState(); err != nil || desired {
		t.Fatalf("empty prepared state desired=%t err=%v", desired, err)
	}
	if _, err := g.recordOpenAIAPIKey("openai-api", "sk-first"); err != nil {
		t.Fatal(err)
	}
	if err := g.PrepareContainer(context.Background(), "10.44.0.1"); err != nil {
		t.Fatal(err)
	}
	first, desired, err := g.ComposeState()
	if err != nil || !desired || first.StatePath != state || first.PublishAddress != "10.44.0.1" || len(first.Digest) != 64 {
		t.Fatalf("first Compose state=%+v desired=%t err=%v", first, desired, err)
	}
	if replayed, err := g.recordOpenAIAPIKey("openai-api", "sk-first"); err != nil || replayed {
		t.Fatalf("replayed=%t err=%v", replayed, err)
	}
	if err := g.PrepareContainer(context.Background(), "10.44.0.1"); err != nil {
		t.Fatal(err)
	}
	replayed, desired, err := g.ComposeState()
	if err != nil || !desired || replayed.Digest != first.Digest {
		t.Fatalf("replayed Compose state=%+v desired=%t err=%v", replayed, desired, err)
	}
	if err := g.PrepareContainer(context.Background(), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	republished, desired, err := g.ComposeState()
	if err != nil || !desired || republished.PublishAddress != "127.0.0.1" || republished.Digest == replayed.Digest {
		t.Fatalf("republished Compose state=%+v desired=%t err=%v", republished, desired, err)
	}
	if err := g.PrepareContainer(context.Background(), "0.0.0.0"); err == nil {
		t.Fatal("wildcard host publish address was accepted")
	}
}

func TestRunForegroundUsesPreparedBrokerUntilContextCancellation(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)
	if err := g.Prepare(context.Background(), "127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "foreground.hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr bytes.Buffer
	result := make(chan error, 1)
	go func() {
		result <- g.RunForeground(ctx, &stdout, &stderr)
	}()
	waitForFile(t, filepath.Join(state, "foreground.started"))

	arguments, err := os.ReadFile(filepath.Join(state, "foreground.args"))
	if err != nil {
		t.Fatal(err)
	}
	wantArguments := "-config " + filepath.Join(state, "broker.yaml") + " -local-model"
	if strings.TrimSpace(string(arguments)) != wantArguments {
		t.Fatalf("foreground arguments=%q, want %q", strings.TrimSpace(string(arguments)), wantArguments)
	}
	if _, err := os.Stat(filepath.Join(state, "broker.pid")); !os.IsNotExist(err) {
		t.Fatalf("foreground process wrote a PID receipt: %v", err)
	}

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("foreground cancellation error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreground broker did not stop after context cancellation")
	}
	waitForFile(t, filepath.Join(state, "foreground.terminated"))
	if got := stdout.String(); !strings.Contains(got, "attached stdout") {
		t.Fatalf("stdout=%q", got)
	}
	if got := stderr.String(); !strings.Contains(got, "attached stderr") {
		t.Fatalf("stderr=%q", got)
	}
}

func TestRunForegroundRefusesBackendChangedAfterPreparation(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)
	if err := g.Prepare(context.Background(), "127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	executable := g.backendExecutable()
	tampered := `#!/bin/sh
if [ "$1" = "-h" ]; then
	printf '%s\n' 'CLIProxyAPI ` + BackendVersion + `'
	exit 0
fi
: > tampered.started
`
	if err := os.WriteFile(executable, []byte(tampered), 0o700); err != nil {
		t.Fatal(err)
	}

	err := g.RunForeground(context.Background(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered backend error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(state, "tampered.started")); !os.IsNotExist(statErr) {
		t.Fatalf("tampered backend was executed: %v", statErr)
	}
}

func TestRunForegroundRefusesBrokerConfigChangedAfterPreparation(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)
	if err := g.Prepare(context.Background(), "127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "broker.yaml"), []byte("host: malicious.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := g.RunForeground(context.Background(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "broker.yaml checksum mismatch") {
		t.Fatalf("tampered broker config error=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(state, "foreground.started")); !os.IsNotExist(statErr) {
		t.Fatalf("broker started with tampered config: %v", statErr)
	}
}

func TestPreparedComposePublishAddressSurvivesMissingCurrentBackend(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)
	if address, found, err := g.PreparedComposePublishAddress(); err != nil || found || address != "" {
		t.Fatalf("absent publish address=%q found=%t error=%v", address, found, err)
	}
	if err := g.PrepareContainer(context.Background(), "10.44.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(g.backendExecutable()); err != nil {
		t.Fatal(err)
	}

	address, found, err := g.PreparedComposePublishAddress()
	if err != nil || !found || address != "10.44.0.1" {
		t.Fatalf("retained publish address=%q found=%t error=%v", address, found, err)
	}
}

func TestPreparedComposePublishAddressRejectsUnprotectedState(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)
	if err := g.PrepareContainer(context.Background(), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(state, "compose.json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := g.PreparedComposePublishAddress(); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("unprotected Compose launch state error=%v", err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func installFakeBackend(t *testing.T, state string) string {
	t.Helper()
	executable := filepath.Join(state, "bin", BackendVersion, "cli-proxy-api")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = "-h" ]; then
	printf '%s\n' 'CLIProxyAPI ` + BackendVersion + `'
	exit 0
fi
printf '%s\n' "$*" > foreground.args
printf '%s\n' 'attached stdout'
printf '%s\n' 'attached stderr' >&2
: > foreground.started
trap 'printf "%s\n" terminated > foreground.terminated; exit 0' TERM INT
while test -e foreground.hold; do
	sleep 0.05
done
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return executable
}

func fakeGateway(t *testing.T, state string) Gateway {
	t.Helper()
	executable := installFakeBackend(t, state)
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return Gateway{StatePath: state, backendSHA256: hex.EncodeToString(digest[:])}
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

func TestOpenAIAPIKeyConnectionIsPrivateIdempotentAndRejectsRotation(t *testing.T) {
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
	beforeConnections, err := os.ReadFile(filepath.Join(g.StatePath, "connections.json"))
	if err != nil {
		t.Fatal(err)
	}
	records, err := g.connections()
	if err != nil || len(records) != 1 {
		t.Fatalf("connections=%v err=%v", records, err)
	}
	credential := filepath.Join(g.StatePath, "credentials", records[0].CredentialRef)
	beforeCredential, err := os.ReadFile(credential)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := g.recordOpenAIAPIKey("openai-api", "sk-another-secret")
	if err == nil || rotated || !strings.Contains(err.Error(), "key rotation is not supported") {
		t.Fatalf("rotated=%t err=%v", rotated, err)
	}
	if _, err := g.recordOpenAIAPIKey("second-openai", "sk-second"); err == nil || !strings.Contains(err.Error(), "one upstream authentication mode") {
		t.Fatalf("ambiguous upstream error=%v", err)
	}
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
	if err != nil || strings.TrimSpace(string(secret)) != "sk-test-secret" {
		t.Fatalf("retained credential=%q err=%v", strings.TrimSpace(string(secret)), err)
	}
	if !bytes.Equal(raw, beforeConnections) || !bytes.Equal(secret, beforeCredential) {
		t.Fatal("rejected key rotation mutated retained connection state")
	}
}

func TestConnectOpenAIAPIKeyRejectsRotationBeforeStateOrConfigMutation(t *testing.T) {
	state := t.TempDir()
	g := fakeGateway(t, state)
	requests := 0
	g.UpstreamClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
	})}
	if err := g.ConnectOpenAIAPIKey(context.Background(), "openai-api", "127.0.0.1", "sk-test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := g.SetDefaultConnection("openai-api"); err != nil {
		t.Fatal(err)
	}
	records, err := g.connections()
	if err != nil || len(records) != 1 {
		t.Fatalf("connections=%v err=%v", records, err)
	}
	paths := []string{
		"authority.json",
		"broker.yaml",
		"compose.json",
		"connections.json",
		"launch.json",
		filepath.Join("credentials", records[0].CredentialRef),
	}
	before := make(map[string][]byte, len(paths))
	for _, relative := range paths {
		before[relative], err = os.ReadFile(filepath.Join(state, relative))
		if err != nil {
			t.Fatal(err)
		}
	}

	err = g.ConnectOpenAIAPIKey(context.Background(), "openai-api", "10.44.0.1", "sk-another-secret")
	if err == nil || !strings.Contains(err.Error(), "key rotation is not supported") {
		t.Fatalf("changed key error=%v", err)
	}
	if requests != 1 {
		t.Fatalf("changed key made %d upstream readiness requests, want 0", requests-1)
	}
	for _, relative := range paths {
		after, readErr := os.ReadFile(filepath.Join(state, relative))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(after, before[relative]) {
			t.Fatalf("changed key mutated %s", relative)
		}
	}
	if defaultConnection, err := g.DefaultConnection(); err != nil || defaultConnection != "openai-api" {
		t.Fatalf("default connection=%q err=%v", defaultConnection, err)
	}

	if err := g.ConnectOpenAIAPIKey(context.Background(), "openai-api", "127.0.0.1", "sk-test-secret"); err != nil {
		t.Fatalf("same-key replay: %v", err)
	}
	if requests != 2 {
		t.Fatalf("same-key replay readiness requests=%d, want 2 total", requests)
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
