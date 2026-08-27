package cloudflare

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testControlHostname = "api.example.com"
	testModelHostname   = "models.example.com"
)

type fakeRunner struct {
	created                bool
	loseCredential         bool
	afterIngressValidation func()
	dnsRouteErr            error
	dnsRouteErrByHostname  map[string]error
	id                     string
	calls                  []string
}

type fakeDNSResolver struct {
	addresses []string
	err       error
	lookups   int
}

func (r *fakeDNSResolver) LookupHost(_ context.Context, _ string) ([]string, error) {
	r.lookups++
	return append([]string(nil), r.addresses...), r.err
}

func (f *fakeRunner) Run(_ context.Context, env []string, _, _ io.Writer, name string, args ...string) error {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	home := envValue(env, "HOME")
	switch {
	case strings.HasSuffix(call, "tunnel login"):
		path := filepath.Join(home, ".cloudflared", "cert.pem")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("temporary broad authority"), 0o600)
	case strings.Contains(call, " create "):
		f.created = true
		if f.loseCredential {
			return nil
		}
		path := filepath.Join(home, ".cloudflared", f.id+".json")
		return os.WriteFile(path, []byte(`{"TunnelID":"`+f.id+`"}`), 0o600)
	case strings.Contains(call, " token ") && strings.Contains(call, " --cred-file "):
		parts := strings.Fields(call)
		for index, part := range parts {
			if part == "--cred-file" && index+1 < len(parts) {
				return os.WriteFile(parts[index+1], []byte(`{"TunnelID":"`+f.id+`"}`), 0o600)
			}
		}
		return fmt.Errorf("token command omitted --cred-file: %q", call)
	case strings.Contains(call, " route dns "):
		if err := f.dnsRouteErrByHostname[args[len(args)-1]]; err != nil {
			return err
		}
		return f.dnsRouteErr
	default:
		return nil
	}
}

func (f *fakeRunner) Output(_ context.Context, _ []string, name string, args ...string) (string, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if strings.Contains(call, " ingress validate") && f.afterIngressValidation != nil {
		f.afterIngressValidation()
	}
	if strings.Contains(call, "tunnel") && strings.Contains(call, " list ") {
		if !f.created {
			return "[]", nil
		}
		raw, _ := json.Marshal([]listedTunnel{{ID: f.id, Name: tunnelNameFromCalls(f.calls)}})
		return string(raw), nil
	}
	return "", nil
}

func preparedTunnel(t *testing.T, _ string) (Tunnel, *fakeRunner, State) {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state")
	binary, digest := installFakeCloudflared(t, statePath)
	runner := &fakeRunner{id: "11111111-2222-3333-4444-555555555555"}
	tunnel := Tunnel{StatePath: statePath, Binary: binary, Runner: runner, Resolver: &fakeDNSResolver{}, binarySHA256: digest}
	state, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return tunnel, runner, state
}

func TestPrepareRejectsInvalidOrEqualExplicitHostnamesBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name            string
		controlHostname string
		modelHostname   string
		messageFragment string
	}{
		{name: "invalid control", controlHostname: "api", modelHostname: testModelHostname, messageFragment: "hostname"},
		{name: "invalid model", controlHostname: testControlHostname, modelHostname: "models", messageFragment: "hostname"},
		{name: "equal after canonicalization", controlHostname: " API.EXAMPLE.COM ", modelHostname: testControlHostname, messageFragment: "must be different"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{}
			_, err := (Tunnel{Runner: runner}).Prepare(context.Background(), test.controlHostname, test.modelHostname, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.messageFragment) {
				t.Fatalf("Prepare error=%v, want fragment %q", err, test.messageFragment)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("invalid hostnames caused Cloudflare calls: %v", runner.calls)
			}
		})
	}
}

func TestPrepareRefusesRetainedExplicitModelHostnameMismatchBeforeMutation(t *testing.T) {
	tunnel, runner, initial := preparedTunnel(t, ComposeGatewayOrigin)
	initialConfig, err := os.ReadFile(initial.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	tunnel.ReplaceExistingDNS = true

	_, err = tunnel.Prepare(context.Background(), testControlHostname, "inference.example.com", io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "already owns Cloudflare model hostname "+testModelHostname) {
		t.Fatalf("retained model hostname mismatch error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("model hostname mismatch caused Cloudflare calls: %v", runner.calls)
	}
	current, found, currentErr := tunnel.Current()
	if currentErr != nil || !found || current != initial {
		t.Fatalf("state mutated after mismatch: current=%+v initial=%+v found=%t error=%v", current, initial, found, currentErr)
	}
	currentConfig, err := os.ReadFile(initial.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(currentConfig) != string(initialConfig) {
		t.Fatalf("config mutated after mismatch:\n%s", currentConfig)
	}
}

func TestGuidedTunnelRefusesHostnameOccupiedImmediatelyBeforeDNSMutation(t *testing.T) {
	tunnel, runner, _ := preparedTunnel(t, ComposeGatewayOrigin)
	resolver := tunnel.Resolver.(*fakeDNSResolver)
	runner.calls = nil
	resolver.lookups = 0
	runner.afterIngressValidation = func() {
		// Another actor claims the hostname after the DNS preflight but before
		// Cloudflare atomically applies the exact Tunnel route.
		runner.dnsRouteErr = fmt.Errorf("hostname record now belongs to another target")
	}

	state, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "route Cloudflare hostname") || !strings.Contains(err.Error(), "another target") {
		t.Fatalf("late hostname occupation error=%v", err)
	}
	if state.DNSConfigured {
		t.Fatalf("occupied hostname recorded as configured: %+v", state)
	}
	if resolver.lookups != 2 {
		t.Fatalf("DNS preflight lookups=%d, want one observation per retained hostname", resolver.lookups)
	}
	if countCall(runner.calls, " ingress validate") != 1 || countCall(runner.calls, " route dns ") != 1 || countCall(runner.calls, "--overwrite-dns") != 0 {
		t.Fatalf("occupied hostname mutation calls=%v", runner.calls)
	}
}

func TestGuidedTunnelExplicitDNSReplacementUsesCloudflareOverwrite(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state")
	binary, digest := installFakeCloudflared(t, statePath)
	runner := &fakeRunner{id: "11111111-2222-3333-4444-555555555555"}
	tunnel := Tunnel{
		StatePath: statePath, Binary: binary,
		Runner: runner, Resolver: &fakeDNSResolver{addresses: []string{"192.0.2.1"}},
		ReplaceExistingDNS: true, binarySHA256: digest,
	}

	state, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err != nil || !state.DNSConfigured {
		t.Fatalf("replacement state=%+v error=%v", state, err)
	}
	if countCall(runner.calls, " route dns --overwrite-dns ") != 2 {
		t.Fatalf("explicit replacement calls=%v", runner.calls)
	}
	for _, hostname := range []string{testControlHostname, testModelHostname} {
		if countCall(runner.calls, " route dns --overwrite-dns "+state.TunnelID+" "+hostname) != 1 {
			t.Fatalf("explicit replacement omitted %s: %v", hostname, runner.calls)
		}
	}
}

func TestGuidedTunnelRetainsReplacementApprovalAcrossInterruptedRoutes(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state")
	binary, digest := installFakeCloudflared(t, statePath)
	runner := &fakeRunner{
		id:                    "11111111-2222-3333-4444-555555555555",
		dnsRouteErrByHostname: map[string]error{testModelHostname: fmt.Errorf("temporary model DNS failure")},
	}
	tunnel := Tunnel{
		StatePath: statePath, Binary: binary,
		Runner: runner, Resolver: &fakeDNSResolver{addresses: []string{"192.0.2.1"}},
		ReplaceExistingDNS: true, binarySHA256: digest,
	}

	partial, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err == nil || !partial.DNSConfigured || partial.ModelDNSConfigured || !partial.DNSReplacementPending {
		t.Fatalf("interrupted replacement state=%+v error=%v", partial, err)
	}
	runner.calls = nil
	runner.dnsRouteErrByHostname = nil
	tunnel.ReplaceExistingDNS = false

	recovered, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err != nil || !recovered.DNSConfigured || !recovered.ModelDNSConfigured || recovered.DNSReplacementPending {
		t.Fatalf("recovered replacement state=%+v error=%v", recovered, err)
	}
	if countCall(runner.calls, " route dns --overwrite-dns "+recovered.TunnelID+" "+testControlHostname) != 0 ||
		countCall(runner.calls, " route dns --overwrite-dns "+recovered.TunnelID+" "+testModelHostname) != 1 {
		t.Fatalf("replacement recovery calls=%v", runner.calls)
	}
}

func TestGuidedTunnelExplicitRepairReplacesAPreviouslyConfiguredRoute(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state")
	binary, digest := installFakeCloudflared(t, statePath)
	resolver := &fakeDNSResolver{}
	runner := &fakeRunner{id: "11111111-2222-3333-4444-555555555555"}
	tunnel := Tunnel{
		StatePath: statePath, Binary: binary,
		Runner: runner, Resolver: resolver, binarySHA256: digest,
	}
	if _, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}

	resolver.addresses = []string{"192.0.2.1"}
	runner.calls = nil
	tunnel.ReplaceExistingDNS = true
	state, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err != nil || !state.DNSConfigured {
		t.Fatalf("repair state=%+v error=%v", state, err)
	}
	if countCall(runner.calls, " route dns --overwrite-dns ") != 2 {
		t.Fatalf("explicit repair calls=%v", runner.calls)
	}
}

func TestGuidedTunnelRetriesOnlyTheUncommittedModelDNSRoute(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state")
	binary, digest := installFakeCloudflared(t, statePath)
	runner := &fakeRunner{
		id:                    "11111111-2222-3333-4444-555555555555",
		dnsRouteErrByHostname: map[string]error{testModelHostname: fmt.Errorf("temporary model DNS failure")},
	}
	tunnel := Tunnel{
		StatePath: statePath, Binary: binary,
		Runner: runner, Resolver: &fakeDNSResolver{addresses: []string{"192.0.2.1"}}, binarySHA256: digest,
	}

	partial, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "route Cloudflare model hostname") {
		t.Fatalf("partial preparation error=%v", err)
	}
	if !partial.DNSConfigured || partial.ModelDNSConfigured {
		t.Fatalf("partial DNS commit=%+v", partial)
	}
	runner.dnsRouteErrByHostname = nil
	runner.calls = nil

	recovered, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.TunnelID != partial.TunnelID || !recovered.DNSConfigured || !recovered.ModelDNSConfigured {
		t.Fatalf("recovered state=%+v, partial=%+v", recovered, partial)
	}
	if countCall(runner.calls, " create ") != 0 || countCall(runner.calls, " route dns "+partial.TunnelID+" "+testControlHostname) != 0 || countCall(runner.calls, " route dns "+partial.TunnelID+" "+testModelHostname) != 1 {
		t.Fatalf("recovery calls=%v", runner.calls)
	}
}

func TestGuidedTunnelReplaysAmbiguousDNSCommitForExactRetainedTunnel(t *testing.T) {
	tunnel, runner, initial := preparedTunnel(t, ComposeGatewayOrigin)
	resolver := tunnel.Resolver.(*fakeDNSResolver)
	// Simulate process loss after the exact route mutation succeeded but before
	// its local completion flag was retained.
	initial.DNSConfigured = false
	if err := tunnel.save(initial); err != nil {
		t.Fatal(err)
	}
	resolver.addresses = []string{"192.0.2.1"}
	resolver.lookups = 0
	runner.calls = nil
	replayed, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("replay exact retained Tunnel: %v", err)
	}
	if replayed.TunnelID != initial.TunnelID || replayed.Hostname != initial.Hostname || !replayed.DNSConfigured {
		t.Fatalf("replayed state=%+v, initial=%+v", replayed, initial)
	}
	wantRoute := " route dns " + initial.TunnelID + " " + initial.Hostname
	if countCall(runner.calls, wantRoute) != 1 || countCall(runner.calls, " create ") != 0 {
		t.Fatalf("exact Tunnel replay calls=%v", runner.calls)
	}
	if countCall(runner.calls, "--overwrite-dns") != 0 {
		t.Fatalf("exact Tunnel replay requested destructive DNS replacement: %v", runner.calls)
	}
}

func TestGuidedTunnelPreparationRetainsRunAuthorityWithoutInstallingAService(t *testing.T) {
	tunnel, runner, state := preparedTunnel(t, ComposeGatewayOrigin)
	if !state.DNSConfigured {
		t.Fatalf("prepared state=%#v", state)
	}
	for _, path := range []string{state.CredentialPath, state.ConfigPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained run authority %q: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(tunnel.StatePath, "management")); !os.IsNotExist(err) {
		t.Fatalf("temporary account authority remains: %v", err)
	}
	for _, forbidden := range []string{"systemctl", "install -m 0644", "sudo"} {
		if countCall(runner.calls, forbidden) != 0 {
			t.Fatalf("preparation invoked host service authority %q: %v", forbidden, runner.calls)
		}
	}
}

func TestPreparedTunnelRunsForegroundWithPinnedBinaryAndRetainedConfig(t *testing.T) {
	tunnel, runner, state := preparedTunnel(t, ComposeGatewayOrigin)
	runner.calls = nil

	if err := tunnel.RunForeground(context.Background(), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{tunnel.Binary, "--no-autoupdate", "--config", state.ConfigPath, "tunnel", "run"}, " ")
	if len(runner.calls) != 1 || runner.calls[0] != want {
		t.Fatalf("foreground calls=%v, want %q", runner.calls, want)
	}
}

func TestPreparedTunnelRefusesBinaryChangedAfterPreparation(t *testing.T) {
	tunnel, runner, _ := preparedTunnel(t, ComposeGatewayOrigin)
	runner.calls = nil
	if err := os.WriteFile(tunnel.Binary, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := tunnel.RunForeground(context.Background(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered cloudflared error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("tampered cloudflared was executed: %v", runner.calls)
	}
}

func TestPreparedTunnelRefusesConfigChangedAfterPreparation(t *testing.T) {
	tunnel, runner, state := preparedTunnel(t, ComposeGatewayOrigin)
	runner.calls = nil
	if err := os.WriteFile(state.ConfigPath, []byte("tunnel: malicious\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := tunnel.RunForeground(context.Background(), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "config checksum mismatch") {
		t.Fatalf("tampered Tunnel config error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("cloudflared ran with tampered config: %v", runner.calls)
	}
}

func TestPrepareRuntimeBinaryUpdatesOnlyRetainedExecutableAuthority(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state")
	legacyPath := filepath.Join(statePath, "bin", "legacy", "cloudflared")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyRaw := []byte("legacy cloudflared")
	if err := os.WriteFile(legacyPath, legacyRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyHash := sha256.Sum256(legacyRaw)
	runner := &fakeRunner{id: "11111111-2222-3333-4444-555555555555"}
	legacy := Tunnel{
		StatePath: statePath, Binary: legacyPath,
		Runner: runner, Resolver: &fakeDNSResolver{}, binarySHA256: hex.EncodeToString(legacyHash[:]),
	}
	if _, err := legacy.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	accountCalls := append([]string(nil), runner.calls...)

	currentPath := filepath.Join(statePath, "bin", "current", "cloudflared")
	if err := os.MkdirAll(filepath.Dir(currentPath), 0o700); err != nil {
		t.Fatal(err)
	}
	currentRaw := []byte("current pinned cloudflared")
	if err := os.WriteFile(currentPath, currentRaw, 0o700); err != nil {
		t.Fatal(err)
	}
	currentHash := sha256.Sum256(currentRaw)
	current := Tunnel{StatePath: statePath, Binary: currentPath, binarySHA256: hex.EncodeToString(currentHash[:])}
	for attempt := 0; attempt < 2; attempt++ {
		found, err := current.PrepareRuntimeBinary(context.Background())
		if err != nil || !found {
			t.Fatalf("attempt %d prepared=%t error=%v", attempt+1, found, err)
		}
	}
	state, found, err := current.Current()
	if err != nil || !found || state.BinaryPath != currentPath {
		t.Fatalf("updated state=%+v found=%t error=%v", state, found, err)
	}
	if strings.Join(runner.calls, "\n") != strings.Join(accountCalls, "\n") {
		t.Fatalf("runtime binary update repeated Cloudflare authority calls: before=%v after=%v", accountCalls, runner.calls)
	}
}

func TestPrepareRuntimeBinaryRepairsConfigAfterStateCommitProcessLoss(t *testing.T) {
	tunnel, runner, state := preparedTunnel(t, ComposeGatewayOrigin)

	// Simulate process loss after Prepare retained the new state authority but
	// before it atomically replaced the old one-origin config.
	legacyConfig := fmt.Sprintf("tunnel: %q\ncredentials-file: %q\nno-autoupdate: true\ningress:\n  - hostname: %q\n    path: ^/\\.dorf/probe/%s$\n    service: http_status:204\n  - hostname: %q\n    path: ^/v1(/.*)?$\n    service: %q\n  - service: http_status:404\n", state.TunnelID, state.CredentialPath, state.Hostname, state.ProbeID, state.Hostname, state.Origin)
	if err := writePrivate(state.ConfigPath, []byte(legacyConfig)); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil

	reconcile := Tunnel{StatePath: tunnel.StatePath, Binary: tunnel.Binary, Runner: runner, binarySHA256: tunnel.binarySHA256}
	found, err := reconcile.PrepareRuntimeBinary(context.Background())
	if err != nil || !found {
		t.Fatalf("repair retained config found=%t error=%v", found, err)
	}
	config, err := os.ReadFile(state.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(config), reconcile.config(state); got != want {
		t.Fatalf("repaired config=%q, want %q", got, want)
	}
	if _, desired, err := reconcile.ComposeState(); err != nil || !desired {
		t.Fatalf("repaired Compose state desired=%t error=%v", desired, err)
	}
	for _, forbidden := range []string{"tunnel login", " create ", " route dns ", " token "} {
		if countCall(runner.calls, forbidden) != 0 {
			t.Fatalf("config recovery repeated Cloudflare authority call %q: %v", forbidden, runner.calls)
		}
	}
}

func TestPreparedTunnelExposesStableComposeState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state")
	binary, digest := installFakeCloudflared(t, statePath)
	tunnel := Tunnel{StatePath: statePath, Binary: binary, Runner: &fakeRunner{id: "11111111-2222-3333-4444-555555555555"}, Resolver: &fakeDNSResolver{}, binarySHA256: digest}
	if _, desired, err := tunnel.ComposeState(); err != nil || desired {
		t.Fatalf("absent Compose state desired=%t err=%v", desired, err)
	}
	if _, err := tunnel.Prepare(context.Background(), testControlHostname, testModelHostname, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	prepared, desired, err := tunnel.ComposeState()
	if err != nil || !desired || prepared.StatePath != tunnel.StatePath || len(prepared.Digest) != 64 {
		t.Fatalf("Compose state=%+v desired=%t err=%v", prepared, desired, err)
	}
	config, err := os.ReadFile(filepath.Join(tunnel.StatePath, "config.yml"))
	if err != nil || !strings.Contains(string(config), `service: "`+ComposeGatewayOrigin+`"`) {
		t.Fatalf("Compose sibling config=%s err=%v", config, err)
	}
}

func TestPreparedTunnelRoutesControlAndModelAPIsThroughOneIdentity(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state")
	binary, digest := installFakeCloudflared(t, statePath)
	runner := &fakeRunner{id: "11111111-2222-3333-4444-555555555555"}
	tunnel := Tunnel{
		StatePath:    statePath,
		Binary:       binary,
		Runner:       runner,
		Resolver:     &fakeDNSResolver{},
		binarySHA256: digest,
	}

	state, err := tunnel.Prepare(context.Background(), " API.EXAMPLE.COM ", " MODELS.EXAMPLE.COM ", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != 1 || state.Hostname != testControlHostname || state.ModelHostname != testModelHostname {
		t.Fatalf("prepared state schema=%d hostnames=%q and %q", state.SchemaVersion, state.Hostname, state.ModelHostname)
	}
	if !state.DNSConfigured || !state.ModelDNSConfigured {
		t.Fatalf("prepared DNS state=%+v", state)
	}
	if countCall(runner.calls, " create ") != 1 || countCall(runner.calls, " route dns "+state.TunnelID+" "+testControlHostname) != 1 || countCall(runner.calls, " route dns "+state.TunnelID+" "+testModelHostname) != 1 {
		t.Fatalf("single Tunnel route calls=%v", runner.calls)
	}
	config, err := os.ReadFile(state.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	wantIngress := "" +
		"  - hostname: \"api.example.com\"\n" +
		"    path: ^/\\.dorf/probe/" + state.ProbeID + "$\n" +
		"    service: http_status:204\n" +
		"  - hostname: \"models.example.com\"\n" +
		"    path: ^/\\.dorf/probe/" + state.ProbeID + "$\n" +
		"    service: http_status:204\n" +
		"  - hostname: \"api.example.com\"\n" +
		"    path: ^/v1(/.*)?$\n" +
		"    service: \"http://control-api:8745\"\n" +
		"  - hostname: \"models.example.com\"\n" +
		"    path: ^/v1(/.*)?$\n" +
		"    service: \"http://provider-gateway:8317\"\n" +
		"  - service: http_status:404\n"
	if !strings.Contains(string(config), wantIngress) {
		t.Fatalf("dual-origin config:\n%s\nwant ingress:\n%s", config, wantIngress)
	}
}

func TestPrepareRejectsRetiredSingleOriginStateBeforeMutation(t *testing.T) {
	tunnel, runner, legacy := preparedTunnel(t, ComposeGatewayOrigin)
	legacy.ModelHostname = ""
	legacy.ModelDNSConfigured = false
	if err := tunnel.save(legacy); err != nil {
		t.Fatal(err)
	}
	legacyConfig := fmt.Sprintf("tunnel: %q\ncredentials-file: %q\nno-autoupdate: true\ningress:\n  - hostname: %q\n    path: ^/\\.dorf/probe/%s$\n    service: http_status:204\n  - hostname: %q\n    path: ^/v1(/.*)?$\n    service: %q\n  - service: http_status:404\n", legacy.TunnelID, legacy.CredentialPath, legacy.Hostname, legacy.ProbeID, legacy.Hostname, legacy.Origin)
	if err := writePrivate(legacy.ConfigPath, []byte(legacyConfig)); err != nil {
		t.Fatal(err)
	}

	current, found, err := tunnel.Current()
	if err != nil || !found || current.ModelHostname != "" {
		t.Fatalf("legacy current=%+v found=%t error=%v", current, found, err)
	}
	if state, ready, err := tunnel.ComposeState(); err != nil || ready || state != (ComposeState{}) {
		t.Fatalf("legacy Compose state=%+v ready=%t error=%v", state, ready, err)
	}
	runner.calls = nil
	if _, err := tunnel.Prepare(context.Background(), legacy.Hostname, testModelHostname, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "retired single-origin") {
		t.Fatalf("retired state error=%v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("retired state performed Cloudflare work: %v", runner.calls)
	}
}

func TestCurrentRejectsModelDNSCommitWithoutItsHostname(t *testing.T) {
	tunnel, _, state := preparedTunnel(t, ComposeGatewayOrigin)
	state.ModelHostname = ""
	if err := tunnel.save(state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tunnel.Current(); err == nil || !strings.Contains(err.Error(), "invalid model hostname") {
		t.Fatalf("impossible model DNS state error=%v", err)
	}
}

func installFakeCloudflared(t *testing.T, statePath string) (string, string) {
	t.Helper()
	binary := filepath.Join(statePath, "bin", BinaryVersion, "cloudflared")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("fake")
	if err := os.WriteFile(binary, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return binary, hex.EncodeToString(digest[:])
}

func TestCurrentRejectsUnprotectedOrLinkedOwnershipState(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "state")
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"schema_version":1,"tunnel_name":"dorf-proof","hostname":"api.example.com","origin":"http://provider-gateway:8317"}`)
	path := filepath.Join(statePath, "state.json")
	if err := os.WriteFile(path, state, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Tunnel{StatePath: statePath}).Current(); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("unprotected state error=%v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if state, desired, err := (Tunnel{StatePath: statePath}).ComposeState(); err != nil || desired || state != (ComposeState{}) {
		t.Fatalf("partial state=%#v desired=%t error=%v", state, desired, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "foreign.json")
	if err := os.WriteFile(target, state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (Tunnel{StatePath: statePath}).Current(); err == nil || !strings.Contains(err.Error(), "protected") {
		t.Fatalf("linked state error=%v", err)
	}
}

func TestCloudflareHostnameContract(t *testing.T) {
	for _, invalid := range []string{"", "https://api.example.com", "api", "127.0.0.1", "api.example.com/path"} {
		if _, err := GatewayURL(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
	got, err := GatewayURL(testModelHostname)
	if err != nil || got != "https://models.example.com/v1" {
		t.Fatalf("url=%q err=%v", got, err)
	}
}

func TestCloudflarePublicOriginsUseExactHostnames(t *testing.T) {
	controlURL, err := ControlURL(testControlHostname)
	if err != nil || controlURL != "https://api.example.com" {
		t.Fatalf("control URL=%q err=%v", controlURL, err)
	}
	modelURL, err := GatewayURL(testModelHostname)
	if err != nil || modelURL != "https://models.example.com/v1" {
		t.Fatalf("model Gateway URL=%q err=%v", modelURL, err)
	}
}

func TestCloudflareDeploymentProbeURLRequiresExactStateIdentity(t *testing.T) {
	state := State{Hostname: testControlHostname, ModelHostname: testModelHostname, ProbeID: strings.Repeat("a", 32)}
	for hostname, want := range map[string]string{
		testControlHostname: "https://api.example.com/.dorf/probe/" + state.ProbeID,
		testModelHostname:   "https://models.example.com/.dorf/probe/" + state.ProbeID,
	} {
		if got, err := state.ProbeURL(hostname); err != nil || got != want {
			t.Fatalf("probe URL for %s=%q want=%q err=%v", hostname, got, want, err)
		}
	}
	if _, err := state.ProbeURL("foreign.example.com"); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("foreign probe error=%v", err)
	}
	state.ProbeID = ""
	if _, err := state.ProbeURL(testModelHostname); err == nil || !strings.Contains(err.Error(), "rerun dorf setup") {
		t.Fatalf("missing identity error=%v", err)
	}
}

func TestCloudflareManagementHomeReplacesTheCallerHome(t *testing.T) {
	merged := mergeEnv([]string{"HOME=/home/operator", "PATH=/usr/bin"}, []string{"HOME=/private/management"})
	if got := envValue(merged, "HOME"); got != "/private/management" {
		t.Fatalf("HOME=%q env=%v", got, merged)
	}
	if countCall(merged, "HOME=") != 1 {
		t.Fatalf("duplicate HOME could leak the account certificate: %v", merged)
	}
}

func TestExecRunnerKeepsDiagnosticStderrOutOfMachineReadableOutput(t *testing.T) {
	output, err := (ExecRunner{}).Output(context.Background(), nil, "sh", "-c", `printf '[{"id":"one"}]'; printf '{"level":"info"}' >&2`)
	if err != nil {
		t.Fatal(err)
	}
	if output != `[{"id":"one"}]` {
		t.Fatalf("output=%q", output)
	}
	_, err = (ExecRunner{}).Output(context.Background(), nil, "sh", "-c", `printf 'useful failure' >&2; exit 7`)
	if err == nil || !strings.Contains(err.Error(), "useful failure") {
		t.Fatalf("error=%v", err)
	}
}

func envValue(env []string, key string) string {
	for _, value := range env {
		if strings.HasPrefix(value, key+"=") {
			return strings.TrimPrefix(value, key+"=")
		}
	}
	return ""
}

func tunnelNameFromCalls(calls []string) string {
	for _, call := range calls {
		parts := strings.Fields(call)
		for index, part := range parts {
			if part == "create" && index+1 < len(parts) {
				return parts[index+1]
			}
		}
	}
	return ""
}

func countCall(calls []string, fragment string) int {
	count := 0
	for _, call := range calls {
		if strings.Contains(call, fragment) {
			count++
		}
	}
	return count
}
