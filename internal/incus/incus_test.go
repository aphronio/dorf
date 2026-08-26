package incus

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type fakeFactory struct {
	mu      sync.Mutex
	client  *fakeClient
	opens   int
	configs []ConnectionConfig
}

func (f *fakeFactory) Open(_ context.Context, config ConnectionConfig) (Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	f.configs = append(f.configs, config)
	return f.client, nil
}

type fakeClient struct {
	mu             sync.Mutex
	instances      map[string]Instance
	createErr      error
	creates        []CreateInstanceRequest
	starts         int
	deletes        int
	execCalls      [][]string
	execResult     Result
	networkIPv4    string
	forwardCalls   int
	forwardAddress string
	forwardPort    int
	forward        func(context.Context) (net.Conn, error)
}

func newFakeClient(instances ...Instance) *fakeClient {
	client := &fakeClient{instances: map[string]Instance{}}
	for _, instance := range instances {
		client.instances[instance.Name] = cloneInstance(instance)
	}
	return client
}

func (c *fakeClient) Instances(context.Context) ([]Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]Instance, 0, len(c.instances))
	for _, instance := range c.instances {
		result = append(result, cloneInstance(instance))
	}
	return result, nil
}

func (c *fakeClient) Instance(_ context.Context, name string) (Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	instance, ok := c.instances[name]
	if !ok {
		return Instance{}, ErrNotFound
	}
	return cloneInstance(instance), nil
}

func (c *fakeClient) CreateInstance(_ context.Context, request CreateInstanceRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.createErr != nil {
		return c.createErr
	}
	c.creates = append(c.creates, request)
	c.instances[request.Name] = Instance{Name: request.Name, Config: cloneStrings(request.Config)}
	return nil
}

func (c *fakeClient) PatchInstanceConfig(_ context.Context, name string, required, updates map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	instance, ok := c.instances[name]
	if !ok {
		return ErrNotFound
	}
	for key, value := range required {
		if instance.Config[key] != value {
			return ownershipErrorf("Incus instance metadata %s does not match its durable owner", key)
		}
	}
	instance.Config = cloneStrings(instance.Config)
	for key, value := range updates {
		instance.Config[key] = value
	}
	c.instances[name] = instance
	return nil
}

func (c *fakeClient) StartInstance(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	instance := c.instances[name]
	instance.Running = true
	c.instances[name] = instance
	c.starts++
	return nil
}

func (c *fakeClient) DeleteInstance(_ context.Context, name string, required map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, value := range required {
		if c.instances[name].Config[key] != value {
			return ownershipErrorf("Incus instance metadata %s does not match its durable owner", key)
		}
	}
	delete(c.instances, name)
	c.deletes++
	return nil
}

func (c *fakeClient) Exec(_ context.Context, _ string, _ []byte, args ...string) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execCalls = append(c.execCalls, append([]string(nil), args...))
	return c.execResult, nil
}

func (c *fakeClient) NetworkIPv4(context.Context, string) (string, error) {
	return c.networkIPv4, nil
}

func (c *fakeClient) OpenPortForward(ctx context.Context, _ string, address string, port int) (net.Conn, error) {
	c.mu.Lock()
	c.forwardCalls++
	c.forwardAddress, c.forwardPort = address, port
	forward := c.forward
	c.mu.Unlock()
	if forward == nil {
		return nil, errors.New("unexpected port forward")
	}
	return forward(ctx)
}

func (*fakeClient) Close() {}

func cloneInstance(instance Instance) Instance {
	instance.Config = cloneStrings(instance.Config)
	return instance
}

func ownedInstance(owner OwnershipMetadata) Instance {
	return Instance{Name: owner.SandboxID, Config: ownershipConfig(owner), Running: true}
}

func TestCreateClassifiesOnlyMissingImageAsUnavailableProfileArtifact(t *testing.T) {
	owner := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	for _, test := range []struct {
		name        string
		createErr   error
		unavailable bool
	}{
		{name: "missing image", createErr: errors.New(`create Incus instance: image "missing" not found`), unavailable: true},
		{name: "other create failure", createErr: errors.New("network unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newFakeClient()
			client.createErr = test.createErr
			sandbox := Sandbox{Config: Config{Image: "missing", Network: "incusbr0", DiskSize: "40GiB"}, ClientFactory: &fakeFactory{client: client}}
			err := sandbox.ReconcileOwnedCreate(context.Background(), owner)
			if provider.IsArtifactUnavailable(err) != test.unavailable {
				t.Fatalf("unavailable=%v error=%v", provider.IsArtifactUnavailable(err), err)
			}
		})
	}
}

func TestSandboxRequiresExactDurableOwnership(t *testing.T) {
	owner := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	client := newFakeClient(ownedInstance(owner))
	sandbox := Sandbox{ClientFactory: &fakeFactory{client: client}}
	if err := sandbox.AttestOwnership(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	wrong := owner
	wrong.OwnershipNonce = strings.Repeat("c", 64)
	if err := sandbox.AttestOwnership(context.Background(), wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched owner metadata error=%v", err)
	}
	competing := ownedInstance(owner)
	competing.Name = "dorf-competing"
	client.instances[competing.Name] = competing
	if err := sandbox.AttestOwnership(context.Background(), owner); err == nil {
		t.Fatal("ambiguous Sandbox was accepted")
	}
}

func TestSandboxDeletionIsRetrySafeButNeverDeletesForeignMetadata(t *testing.T) {
	owner := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	client := newFakeClient()
	sandbox := Sandbox{ClientFactory: &fakeFactory{client: client}}
	for range 2 {
		if err := sandbox.DeleteOwned(context.Background(), owner); err != nil {
			t.Fatal(err)
		}
	}
	client.instances[owner.SandboxID] = Instance{Name: owner.SandboxID, Config: map[string]string{"user.dorf.sandbox": owner.SandboxID, "user.dorf.owner": "foreign"}}
	if err := sandbox.DeleteOwned(context.Background(), owner); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("foreign Sandbox deletion error=%v", err)
	}
}

func TestOwnedSandboxCreationUsesRecordedIdentityAndCredentialFreeBoundary(t *testing.T) {
	client := newFakeClient()
	factory := &fakeFactory{client: client}
	sandbox := Sandbox{Config: Config{Image: "dorf-codex", Network: "incusbr0", DiskSize: "40GiB", Workspace: "/workspace/job"}, ClientFactory: factory, Sleep: func(time.Duration) {}}
	owner := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-sandbox-exact", OwnershipNonce: strings.Repeat("a", 64)}
	if err := sandbox.ReconcileOwnedCreate(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.ReconcileOwnedCreate(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if len(client.creates) != 1 || client.creates[0].Config["user.dorf.ownership_nonce"] != owner.OwnershipNonce || client.creates[0].StoragePool != DefaultStoragePool {
		t.Fatalf("creates=%#v", client.creates)
	}
	credentialChecks := 0
	for _, call := range client.execCalls {
		if strings.Contains(strings.Join(call, " "), "auth.json") {
			credentialChecks++
		}
	}
	if credentialChecks != 2 {
		t.Fatalf("credential checks=%d", credentialChecks)
	}
}

func TestConfiguredBridgeIPv4ComesFromExactIncusNetwork(t *testing.T) {
	client := newFakeClient()
	client.networkIPv4 = "10.42.0.1/24"
	sandbox := Sandbox{Config: Config{Network: "dorfbr0"}, ClientFactory: &fakeFactory{client: client}}
	address, err := sandbox.BridgeIPv4(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if address != "10.42.0.1" {
		t.Fatalf("bridge address=%q", address)
	}
}

func TestDefaultConnectionUsesDedicatedRestrictedProject(t *testing.T) {
	connection := DefaultConnectionConfig()
	if connection.Endpoint != "unix:///var/lib/incus/unix.socket" || connection.Project != "dorf" || connection.StoragePool != "default" {
		t.Fatalf("default Incus connection=%#v", connection)
	}
}

func TestPortForwardEndpointIsFailClosedAndOpensAFreshOwnedStream(t *testing.T) {
	owner := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	client := newFakeClient(ownedInstance(owner))
	var peers []net.Conn
	client.forward = func(context.Context) (net.Conn, error) {
		connection, peer := net.Pipe()
		peers = append(peers, peer)
		return connection, nil
	}
	t.Cleanup(func() {
		for _, peer := range peers {
			_ = peer.Close()
		}
	})
	factory := &fakeFactory{client: client}
	sandbox := Sandbox{ClientFactory: factory}
	endpoint, err := sandbox.PortForwardEndpoint(context.Background(), owner, 4500)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.ListenURL != "ws://127.0.0.1:4500" || endpoint.DialURL != "ws://incus.invalid:4500" {
		t.Fatalf("endpoint=%#v", endpoint)
	}
	if _, err := endpoint.DialContext()(context.Background(), "tcp", "foreign.example:4500"); err == nil {
		t.Fatal("synthetic endpoint dialer accepted a foreign target")
	}
	first, err := endpoint.DialContext()(context.Background(), "tcp", "incus.invalid:4500")
	if err != nil {
		t.Fatal(err)
	}
	second, err := endpoint.DialContext()(context.Background(), "tcp", "incus.invalid:4500")
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	_ = second.Close()
	if factory.opens != 3 || client.forwardCalls != 2 || client.forwardAddress != "127.0.0.1" || client.forwardPort != 4500 {
		t.Fatalf("opens=%d forwards=%d target=%s:%d", factory.opens, client.forwardCalls, client.forwardAddress, client.forwardPort)
	}
}

func TestPortForwardEndpointPropagatesDialCancellation(t *testing.T) {
	owner := OwnershipMetadata{JobID: "job-1", SandboxID: "dorf-owned", OwnershipNonce: strings.Repeat("b", 64)}
	client := newFakeClient(ownedInstance(owner))
	client.forward = func(ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	endpoint, err := (Sandbox{ClientFactory: &fakeFactory{client: client}}).PortForwardEndpoint(context.Background(), owner, 4500)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = endpoint.DialContext()(ctx, "tcp", "incus.invalid:4500")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error=%v", err)
	}
}

var _ ClientFactory = (*fakeFactory)(nil)
var _ Client = (*fakeClient)(nil)
