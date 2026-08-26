package incus

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

func TestConnectionConfigAcceptsOnlyExplicitLocalOrPinnedMTLSRemote(t *testing.T) {
	local := ConnectionConfig{
		Endpoint:    "unix:///var/lib/incus/unix.socket",
		Project:     "dorf",
		StoragePool: "default",
	}
	if err := local.Validate(); err != nil {
		t.Fatalf("valid local connection: %v", err)
	}

	serverCert, _ := testCertificate(t, "incus.example")
	clientCert, clientKey := testCertificate(t, "dorf-worker")
	remote := ConnectionConfig{
		Endpoint:             "https://incus.example:8443",
		Project:              "dorf",
		StoragePool:          "default",
		TLSServerCertificate: serverCert,
		TLSClientCertificate: clientCert,
		TLSClientKey:         clientKey,
	}
	if err := remote.Validate(); err != nil {
		t.Fatalf("valid remote connection: %v", err)
	}

	for _, test := range []struct {
		name   string
		change func(*ConnectionConfig)
		want   string
	}{
		{name: "ambient endpoint", change: func(c *ConnectionConfig) { c.Endpoint = "" }, want: "endpoint"},
		{name: "relative unix socket", change: func(c *ConnectionConfig) { c.Endpoint = "unix://relative/socket" }, want: "absolute"},
		{name: "remote HTTP", change: func(c *ConnectionConfig) { c.Endpoint = "http://incus.example:8443" }, want: "unix or https"},
		{name: "remote path", change: func(c *ConnectionConfig) { c.Endpoint = "https://incus.example:8443/1.0" }, want: "origin"},
		{name: "missing server pin", change: func(c *ConnectionConfig) { c.TLSServerCertificate = "" }, want: "server certificate"},
		{name: "missing client certificate", change: func(c *ConnectionConfig) { c.TLSClientCertificate = "" }, want: "client certificate"},
		{name: "missing client key", change: func(c *ConnectionConfig) { c.TLSClientKey = "" }, want: "client key"},
		{name: "missing project", change: func(c *ConnectionConfig) { c.Project = "" }, want: "project"},
		{name: "non-canonical project", change: func(c *ConnectionConfig) { c.Project = " dorf " }, want: "project"},
		{name: "missing storage pool", change: func(c *ConnectionConfig) { c.StoragePool = "" }, want: "storage pool"},
		{name: "non-canonical storage pool", change: func(c *ConnectionConfig) { c.StoragePool = " default " }, want: "storage pool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := remote
			if strings.Contains(test.name, "unix") || test.name == "ambient endpoint" {
				candidate = local
			}
			test.change(&candidate)
			err := candidate.Validate()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.want) {
				t.Fatalf("validation error=%v, want %q", err, test.want)
			}
		})
	}
}

type fakeSDKServer struct {
	incusclient.InstanceServer
	extension    bool
	project      string
	disconnected bool
	context      context.Context
	instance     api.Instance
	stop         api.InstanceStatePut
	stopETag     string
	stopOp       *fakeOperation
	deleteOp     *fakeOperation
	deletes      int
	forwardStart chan struct{}
	forwardReady chan struct{}
	forwardConn  net.Conn
}

func (s *fakeSDKServer) HasExtension(extension string) bool {
	return extension == requiredPortForwardExtension && s.extension
}

func (s *fakeSDKServer) UseProject(project string) incusclient.InstanceServer {
	s.project = project
	return s
}

func (s *fakeSDKServer) WithContext(ctx context.Context) incusclient.InstanceServer {
	s.context = ctx
	return s
}
func (s *fakeSDKServer) Disconnect() { s.disconnected = true }

func (*fakeSDKServer) GetInstances(api.InstanceType) ([]api.Instance, error) { return nil, nil }

func (s *fakeSDKServer) GetInstance(string) (*api.Instance, string, error) {
	instance := s.instance
	return &instance, "exact-etag", nil
}

func (s *fakeSDKServer) UpdateInstanceState(_ string, state api.InstanceStatePut, etag string) (incusclient.Operation, error) {
	s.stop = state
	s.stopETag = etag
	return s.stopOp, nil
}

func (s *fakeSDKServer) DeleteInstance(string) (incusclient.Operation, error) {
	s.deletes++
	return s.deleteOp, nil
}

func (s *fakeSDKServer) GetInstancePortForwardConn(string, api.InstancePortForwardPost) (net.Conn, error) {
	close(s.forwardStart)
	<-s.forwardReady
	return s.forwardConn, nil
}

type fakeOperation struct {
	incusclient.Operation
	waited context.Context
	onWait func()
}

func (o *fakeOperation) WaitContext(ctx context.Context) error {
	o.waited = ctx
	if o.onWait != nil {
		o.onWait()
	}
	return nil
}

func (*fakeOperation) Get() api.Operation { return api.Operation{} }

func TestSDKClientBindsEveryCallAndOperationWaitToItsContext(t *testing.T) {
	server := &fakeSDKServer{}
	client := &sdkClient{server: server}
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("scope"), "exact-call")
	if _, err := client.Instances(ctx); err != nil {
		t.Fatal(err)
	}
	if server.context != ctx {
		t.Fatal("SDK call did not use its per-call context")
	}
}

func TestSDKDeleteForceStopsRunningInstanceBeforeDelete(t *testing.T) {
	stop, deletion := &fakeOperation{}, &fakeOperation{}
	server := &fakeSDKServer{
		instance: api.Instance{Name: "dorf-owned", StatusCode: api.Running, InstancePut: api.InstancePut{Config: api.ConfigMap{"user.dorf.owner": "sandbox"}}},
		stopOp:   stop, deleteOp: deletion,
	}
	stop.onWait = func() { server.instance.StatusCode = api.Stopped }
	client := &sdkClient{server: server}
	ctx := context.Background()
	if err := client.DeleteInstance(ctx, "dorf-owned", map[string]string{"user.dorf.owner": "sandbox"}); err != nil {
		t.Fatal(err)
	}
	if server.stop.Action != "stop" || !server.stop.Force || server.stop.Timeout != -1 || server.stopETag != "exact-etag" || stop.waited != ctx || server.deletes != 1 || deletion.waited != ctx {
		t.Fatalf("stop=%#v stop-wait=%v deletes=%d delete-wait=%v", server.stop, stop.waited, server.deletes, deletion.waited)
	}
	server.instance.Config["user.dorf.owner"] = "foreign"
	if err := client.DeleteInstance(ctx, "dorf-owned", map[string]string{"user.dorf.owner": "sandbox"}); err == nil || server.deletes != 1 {
		t.Fatalf("foreign replacement delete error=%v deletes=%d", err, server.deletes)
	}
}

func TestSDKDeleteReattestsOwnershipAfterForceStop(t *testing.T) {
	server := &fakeSDKServer{
		instance: api.Instance{Name: "dorf-owned", StatusCode: api.Running, InstancePut: api.InstancePut{Config: api.ConfigMap{"user.dorf.owner": "sandbox"}}},
		deleteOp: &fakeOperation{},
	}
	server.stopOp = &fakeOperation{onWait: func() {
		server.instance.StatusCode = api.Stopped
		server.instance.Config["user.dorf.owner"] = "foreign"
	}}
	client := &sdkClient{server: server}
	err := client.DeleteInstance(context.Background(), "dorf-owned", map[string]string{"user.dorf.owner": "sandbox"})
	if err == nil || server.deletes != 0 {
		t.Fatalf("post-stop ownership error=%v deletes=%d", err, server.deletes)
	}
}

func TestSDKFactoryUsesExactSocketProjectAndRequiredExtension(t *testing.T) {
	server := &fakeSDKServer{extension: true}
	var socket string
	var args *incusclient.ConnectionArgs
	factory := SDKClientFactory{
		connectUnix: func(ctx context.Context, path string, actual *incusclient.ConnectionArgs) (incusclient.InstanceServer, error) {
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > connectTimeout {
				t.Fatal("Incus connection was not bounded")
			}
			socket, args = path, actual
			return server, nil
		},
		connectHTTPS: func(context.Context, string, *incusclient.ConnectionArgs) (incusclient.InstanceServer, error) {
			t.Fatal("local connection used HTTPS connector")
			return nil, nil
		},
	}
	client, err := factory.Open(context.Background(), ConnectionConfig{
		Endpoint: "unix:///run/incus/dorf.socket", Project: "dorf", StoragePool: "dorf-pool",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if socket != "/run/incus/dorf.socket" || args == nil || !args.SkipGetEvents || args.SkipGetServer || server.project != "dorf" || !server.disconnected {
		t.Fatalf("socket=%q args=%#v project=%q disconnected=%v", socket, args, server.project, server.disconnected)
	}

	missing := &fakeSDKServer{}
	factory.connectUnix = func(context.Context, string, *incusclient.ConnectionArgs) (incusclient.InstanceServer, error) {
		return missing, nil
	}
	_, err = factory.Open(context.Background(), ConnectionConfig{
		Endpoint: "unix:///run/incus/dorf.socket", Project: "dorf", StoragePool: "dorf-pool",
	})
	if err == nil || !strings.Contains(err.Error(), requiredPortForwardExtension) || !missing.disconnected {
		t.Fatalf("extension preflight error=%v disconnected=%v", err, missing.disconnected)
	}
}

func TestSDKFactoryPinsRemoteMTLSAndDisablesAmbientProxy(t *testing.T) {
	serverCert, _ := testCertificate(t, "incus.example")
	clientCert, clientKey := testCertificate(t, "dorf-worker")
	server := &fakeSDKServer{extension: true}
	var endpoint string
	var args *incusclient.ConnectionArgs
	factory := SDKClientFactory{
		connectHTTPS: func(_ context.Context, actualEndpoint string, actualArgs *incusclient.ConnectionArgs) (incusclient.InstanceServer, error) {
			endpoint, args = actualEndpoint, actualArgs
			return server, nil
		},
		connectUnix: func(context.Context, string, *incusclient.ConnectionArgs) (incusclient.InstanceServer, error) {
			t.Fatal("remote connection used Unix connector")
			return nil, nil
		},
	}
	client, err := factory.Open(context.Background(), ConnectionConfig{
		Endpoint: "  https://incus.example:8443/  ", Project: "dorf", StoragePool: "dorf-pool",
		TLSServerCertificate: serverCert, TLSClientCertificate: clientCert, TLSClientKey: clientKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	if endpoint != "https://incus.example:8443" || args == nil || args.TLSServerCert != serverCert || args.TLSClientCert != clientCert || args.TLSClientKey != clientKey || !args.IdenticalCertificate || args.InsecureSkipVerify || !args.SkipGetEvents || args.SkipGetServer {
		t.Fatalf("endpoint=%q args=%#v", endpoint, args)
	}
	proxy, err := args.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "incus.example:8443"}})
	if err != nil || proxy != nil {
		t.Fatalf("proxy=%v err=%v", proxy, err)
	}
}

func TestSDKPortForwardReturnsOnCancellationAndClosesLateConnection(t *testing.T) {
	connection, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	server := &fakeSDKServer{
		forwardStart: make(chan struct{}),
		forwardReady: make(chan struct{}),
		forwardConn:  connection,
	}
	client := &sdkClient{server: server}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.OpenPortForward(ctx, "dorf-owned", "127.0.0.1", 4500)
		result <- err
	}()
	<-server.forwardStart
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("dial error=%v", err)
	}
	if !server.disconnected {
		t.Fatal("canceled port-forward did not disconnect the SDK client")
	}
	close(server.forwardReady)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection returned after cancellation was not closed")
	}
}

func testCertificate(t *testing.T, commonName string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}
