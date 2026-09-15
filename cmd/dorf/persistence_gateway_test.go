package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/persistence"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/terminal"
)

// livePersistenceGatewayRecovery exercises the production route renewal and
// native verification method on the replacement used by the persistence live
// proof. The Gateway control plane is a deterministic host fixture: no model
// credential or inference request enters the proof.
type livePersistenceGatewayRecovery struct {
	checkpointRecovery
	fixture        []byte
	gateway        *recoveryGatewayFixture
	verifyAttempts int
	firstKeyDigest string
}

func newLivePersistenceGatewayRecovery(t *testing.T, recovery checkpointRecovery, fixture []byte) *livePersistenceGatewayRecovery {
	t.Helper()
	controlled := newRecoveryGatewayFixture(t)
	recovery.externals = terminal.Externals{
		Sandbox: recovery.capture.sandbox,
		Agent:   recovery.capture.agent,
		Gateway: controlled.gateway(),
	}
	return &livePersistenceGatewayRecovery{checkpointRecovery: recovery, fixture: fixture, gateway: controlled}
}

func (d *livePersistenceGatewayRecovery) VerifyAndRenew(ctx context.Context, job core.Job, destination core.Sandbox, checkpoint persistence.Checkpoint, pkg persistence.EffectivePackage, runs []core.AgentRun) error {
	d.verifyAttempts++
	owner := livePersistenceOwner(destination)
	if err := d.assertReplacementState(ctx, owner); err != nil {
		return err
	}
	if err := d.checkpointRecovery.VerifyAndRenew(ctx, job, destination, checkpoint, pkg, runs); err != nil {
		return err
	}
	digest, err := routeKeyDigest(ctx, d.capture.sandbox, owner)
	if err != nil {
		return err
	}
	if err := d.gateway.assertFreshRoute(d.verifyAttempts); err != nil {
		return err
	}
	if d.verifyAttempts == 1 {
		d.firstKeyDigest = digest
		return errLivePersistenceLostVerificationReceipt
	}
	if d.verifyAttempts != 2 || digest == d.firstKeyDigest {
		return fmt.Errorf("verification replay did not rotate fresh route authority")
	}
	if err := d.capture.agent.Quiesce(ctx, owner, runs); err != nil {
		return err
	}
	if err := d.gateway.revoke(ctx, destination); err != nil {
		return err
	}
	if err := d.capture.agent.RemoveRoute(ctx, owner); err != nil {
		return err
	}
	return installLivePersistenceFixture(ctx, d.capture.sandbox, owner, d.fixture, "synthetic-recovered-route")
}

func (d *livePersistenceGatewayRecovery) assertReplacementState(ctx context.Context, owner provider.Ownership) error {
	script := `test ! -e /root/.codex/config.toml
test ! -e /root/.config/dorf/provider-route.key
test ! -e /tmp/dorf/codex-app-server.pid`
	if d.verifyAttempts > 1 {
		script = `test -s /root/.codex/config.toml
test -s /root/.config/dorf/provider-route.key
test -s /tmp/dorf/codex-app-server.pid`
	}
	result, err := d.capture.sandbox.Exec(ctx, owner, nil, "bash", "-c", script)
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("replacement route and app-server replay state is invalid")
	}
	return nil
}

func routeKeyDigest(ctx context.Context, sandbox provider.Sandbox, owner provider.Ownership) (string, error) {
	result, err := sandbox.Exec(ctx, owner, nil, "sha256sum", "/root/.config/dorf/provider-route.key")
	if err != nil || result.ExitCode != 0 {
		return "", fmt.Errorf("read replacement route authority digest")
	}
	digest := strings.Fields(result.Stdout)
	if len(digest) != 2 || len(digest[0]) != 64 {
		return "", fmt.Errorf("replacement route authority digest is invalid")
	}
	if _, err := hex.DecodeString(digest[0]); err != nil {
		return "", fmt.Errorf("replacement route authority digest is invalid")
	}
	return digest[0], nil
}

type recoveryGatewayFixture struct {
	state      string
	guard      string
	management string
	model      string
	mu         sync.Mutex
	active     map[string]bool
	issued     []string
}

func newRecoveryGatewayFixture(t *testing.T) *recoveryGatewayFixture {
	t.Helper()
	fixture := &recoveryGatewayFixture{
		state: t.TempDir(), guard: "synthetic-guard", management: "synthetic-management",
		model: "synthetic-model", active: map[string]bool{},
	}
	if err := os.Mkdir(filepath.Join(fixture.state, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"credentials/openai-0123456789abcdef.key": "synthetic-provider-key\n",
		"connections.json":                        `[{"name":"synthetic-local","provider":"openai","auth_mode":"api_key","credential_ref":"openai-0123456789abcdef.key","default":true}]`,
		"authority.json":                          `{"guard_key":"synthetic-guard","management_key":"synthetic-management"}`,
		"broker.yaml":                             "host: \"gateway-proof.invalid\"\nport: 8317\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(fixture.state, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return fixture
}

func (f *recoveryGatewayFixture) gateway() gateway.Gateway {
	return gateway.Gateway{StatePath: f.state, Client: &http.Client{Transport: recoveryGatewayTransport{fixture: f}}}
}

func (f *recoveryGatewayFixture) respond(request *http.Request) (*http.Response, error) {
	switch {
	case request.Method == http.MethodPut && request.URL.Path == "/v0/management/api-keys":
		if request.Header.Get("Authorization") != "Bearer "+f.management {
			return recoveryGatewayResponse(http.StatusForbidden, ""), nil
		}
		var keys []string
		if err := json.NewDecoder(request.Body).Decode(&keys); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.active = make(map[string]bool, len(keys))
		for _, key := range keys {
			f.active[key] = true
		}
		f.mu.Unlock()
		return recoveryGatewayResponse(http.StatusOK, ""), nil
	case request.Method == http.MethodGet && request.URL.Path == "/v1/models":
		key := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		active := f.active[key] && key != f.guard
		if active {
			f.issued = append(f.issued, key)
		}
		f.mu.Unlock()
		if !active {
			return recoveryGatewayResponse(http.StatusUnauthorized, ""), nil
		}
		return recoveryGatewayResponse(http.StatusOK, fmt.Sprintf(`{"data":[{"id":%q}]}`, f.model)), nil
	default:
		return recoveryGatewayResponse(http.StatusNotFound, ""), nil
	}
}

func (f *recoveryGatewayFixture) assertFreshRoute(attempt int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.issued) != attempt || !f.active[f.issued[attempt-1]] {
		return fmt.Errorf("production Gateway omitted active scoped route authority")
	}
	if attempt > 1 && f.issued[attempt-1] == f.issued[attempt-2] {
		return fmt.Errorf("production Gateway reused revoked route authority")
	}
	if attempt > 1 && f.active[f.issued[attempt-2]] {
		return fmt.Errorf("production Gateway retained prior route authority")
	}
	return nil
}

func (f *recoveryGatewayFixture) revoke(ctx context.Context, sandbox core.Sandbox) error {
	route := core.RouteForSandbox(sandbox)
	if err := f.gateway().RevokeExact(ctx, "sandbox:"+sandbox.ID, route.ID); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, key := range f.issued {
		if f.active[key] {
			return fmt.Errorf("proof cleanup retained scoped route authority")
		}
	}
	return nil
}

type recoveryGatewayTransport struct{ fixture *recoveryGatewayFixture }

func (t recoveryGatewayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return t.fixture.respond(request)
}

func recoveryGatewayResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
