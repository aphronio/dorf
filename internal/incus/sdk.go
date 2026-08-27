package incus

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

const (
	requiredPortForwardExtension = "instance_port_forward"
	connectTimeout               = 10 * time.Second
	portForwardDialTimeout       = 10 * time.Second
)

type SDKClientFactory struct {
	connectUnix  func(context.Context, string, *incusclient.ConnectionArgs) (incusclient.InstanceServer, error)
	connectHTTPS func(context.Context, string, *incusclient.ConnectionArgs) (incusclient.InstanceServer, error)
}

func (f SDKClientFactory) Open(ctx context.Context, config ConnectionConfig) (Client, error) {
	server, err := f.openServer(ctx, config)
	if err != nil {
		return nil, err
	}
	if !server.HasExtension(requiredPortForwardExtension) {
		server.Disconnect()
		return nil, fmt.Errorf("Incus endpoint is missing required API extension %q", requiredPortForwardExtension)
	}
	return &sdkClient{server: server}, nil
}

func (f SDKClientFactory) openServer(ctx context.Context, config ConnectionConfig) (incusclient.InstanceServer, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(config.Endpoint)
	parsed, _ := url.Parse(endpoint)
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	args := &incusclient.ConnectionArgs{SkipGetEvents: true}
	var server incusclient.InstanceServer
	var err error
	if parsed.Scheme == "unix" {
		connect := f.connectUnix
		if connect == nil {
			connect = incusclient.ConnectIncusUnixWithContext
		}
		server, err = connect(connectCtx, parsed.Path, args)
	} else {
		args.TLSServerCert = config.TLSServerCertificate
		args.TLSClientCert = config.TLSClientCertificate
		args.TLSClientKey = config.TLSClientKey
		args.IdenticalCertificate = true
		args.Proxy = func(*http.Request) (*url.URL, error) { return nil, nil }
		connect := f.connectHTTPS
		if connect == nil {
			connect = incusclient.ConnectIncusWithContext
		}
		server, err = connect(connectCtx, strings.TrimSuffix(endpoint, "/"), args)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to explicit Incus endpoint: %w", err)
	}
	server = server.UseProject(strings.TrimSpace(config.Project))
	if contextual, ok := server.(interface {
		WithContext(context.Context) incusclient.InstanceServer
	}); ok {
		server = contextual.WithContext(ctx)
	}
	return server, nil
}

type sdkClient struct{ server incusclient.InstanceServer }

func (c *sdkClient) Close() { c.server.Disconnect() }

func (c *sdkClient) serverFor(ctx context.Context) incusclient.InstanceServer {
	if contextual, ok := c.server.(interface {
		WithContext(context.Context) incusclient.InstanceServer
	}); ok {
		return contextual.WithContext(ctx)
	}
	return c.server
}

func (c *sdkClient) Instances(ctx context.Context) ([]Instance, error) {
	instances, err := c.serverFor(ctx).GetInstances(api.InstanceTypeAny)
	if err != nil {
		return nil, fmt.Errorf("list Incus instances: %w", err)
	}
	result := make([]Instance, 0, len(instances))
	for _, instance := range instances {
		result = append(result, instanceFromAPI(instance))
	}
	return result, nil
}

func (c *sdkClient) Instance(ctx context.Context, name string) (Instance, error) {
	instance, _, err := c.serverFor(ctx).GetInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return Instance{}, ErrNotFound
	}
	if err != nil {
		return Instance{}, fmt.Errorf("get Incus instance %s: %w", name, err)
	}
	return instanceFromAPI(*instance), nil
}

func instanceFromAPI(instance api.Instance) Instance {
	return Instance{Name: instance.Name, Config: cloneStrings(instance.Config), Running: instance.IsActive()}
}

func (c *sdkClient) CreateInstance(ctx context.Context, request CreateInstanceRequest) error {
	server := c.serverFor(ctx)
	post := api.InstancesPost{
		Name:   request.Name,
		Type:   api.InstanceTypeVM,
		Source: api.InstanceSource{Type: "image", Fingerprint: request.Image},
		InstancePut: api.InstancePut{
			Config: api.ConfigMap(cloneStrings(request.Config)),
			Devices: api.DevicesMap{
				"eth0": instanceNetworkDevice(request.Network),
				"root": {"type": "disk", "path": "/", "pool": request.StoragePool, "size": request.DiskSize},
			},
		},
	}
	op, err := server.CreateInstance(post)
	if err != nil {
		return fmt.Errorf("create Incus instance %s: %w", request.Name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("create Incus instance %s: %w", request.Name, err)
	}
	return nil
}

func instanceNetworkDevice(network string) map[string]string {
	device := map[string]string{"type": "nic", "name": "eth0", "network": network}
	if network == RemoteNetworkName {
		device["security.ipv4_filtering"] = "true"
		device["security.ipv6_filtering"] = "true"
		device["security.port_isolation"] = "true"
	}
	return device
}

func (c *sdkClient) PatchInstanceConfig(ctx context.Context, name string, required, updates map[string]string) error {
	server := c.serverFor(ctx)
	instance, etag, err := server.GetInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get Incus instance %s for update: %w", name, err)
	}
	for key, value := range required {
		if instance.Config[key] != value {
			return ownershipErrorf("Incus instance metadata %s does not match its durable owner", key)
		}
	}
	writable := instance.Writable()
	writable.Config = api.ConfigMap(cloneStrings(instance.Config))
	for key, value := range updates {
		writable.Config[key] = value
	}
	op, err := server.UpdateInstance(name, writable, etag)
	if err != nil {
		return fmt.Errorf("update Incus instance %s: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("update Incus instance %s: %w", name, err)
	}
	return nil
}

func (c *sdkClient) StartInstance(ctx context.Context, name string) error {
	server := c.serverFor(ctx)
	instance, etag, err := server.GetInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("get Incus instance %s before start: %w", name, err)
	}
	if instance.IsActive() {
		return nil
	}
	op, err := server.UpdateInstanceState(name, api.InstanceStatePut{Action: "start", Timeout: 30}, etag)
	if err != nil {
		return fmt.Errorf("start Incus instance %s: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil {
		return fmt.Errorf("start Incus instance %s: %w", name, err)
	}
	return nil
}

func (c *sdkClient) DeleteInstance(ctx context.Context, name string, required map[string]string) error {
	server := c.serverFor(ctx)
	instance, etag, err := server.GetInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Incus instance %s before delete: %w", name, err)
	}
	if err := attestRequiredConfig(instance.Config, required); err != nil {
		return err
	}
	if instance.IsActive() {
		stop, err := server.UpdateInstanceState(name, api.InstanceStatePut{Action: "stop", Timeout: -1, Force: true}, etag)
		if err != nil {
			return fmt.Errorf("force-stop Incus instance %s: %w", name, err)
		}
		if err := stop.WaitContext(ctx); err != nil {
			return fmt.Errorf("force-stop Incus instance %s: %w", name, err)
		}
	}

	// Incus deletion has no ETag parameter. Re-fetch after the potentially
	// slow stop operation and make the narrowest possible ownership check
	// immediately before the unconditioned delete call.
	instance, _, err = server.GetInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("re-attest Incus instance %s before delete: %w", name, err)
	}
	if err := attestRequiredConfig(instance.Config, required); err != nil {
		return err
	}
	if instance.IsActive() {
		return ownershipErrorf("Incus instance %s changed state before delete", name)
	}
	op, err := server.DeleteInstance(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete Incus instance %s: %w", name, err)
	}
	if err := op.WaitContext(ctx); err != nil && !api.StatusErrorCheck(err, http.StatusNotFound) {
		return fmt.Errorf("delete Incus instance %s: %w", name, err)
	}
	return nil
}

func attestRequiredConfig(actual, required map[string]string) error {
	for key, value := range required {
		if actual[key] != value {
			return ownershipErrorf("Incus instance metadata %s does not match its durable owner", key)
		}
	}
	return nil
}

func (c *sdkClient) Exec(ctx context.Context, name string, input []byte, command ...string) (Result, error) {
	if len(command) == 0 {
		return Result{}, fmt.Errorf("Incus exec command is required")
	}
	var stdout, stderr bytes.Buffer
	dataDone := make(chan bool)
	op, err := c.serverFor(ctx).ExecInstance(name, api.InstanceExecPost{Command: command, WaitForWS: true}, &incusclient.InstanceExecArgs{
		Stdin: bytes.NewReader(input), Stdout: &stdout, Stderr: &stderr, DataDone: dataDone,
	})
	if err != nil {
		return Result{}, fmt.Errorf("execute in Incus instance %s: %w", name, err)
	}
	waitErr := op.WaitContext(ctx)
	if waitErr != nil {
		return Result{}, fmt.Errorf("execute in Incus instance %s: %w", name, waitErr)
	}
	select {
	case <-dataDone:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	exitCode, hasExitCode := operationExitCode(op.Get().Metadata)
	if hasExitCode {
		result.ExitCode = exitCode
		return result, nil
	}
	return Result{}, fmt.Errorf("execute in Incus instance %s: operation omitted process exit status", name)
}

func operationExitCode(metadata map[string]any) (int, bool) {
	value, ok := metadata["return"]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(typed)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func (c *sdkClient) NetworkIPv4(ctx context.Context, name string) (string, error) {
	network, _, err := c.serverFor(ctx).GetNetwork(name)
	if api.StatusErrorCheck(err, http.StatusNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get Incus network %s: %w", name, err)
	}
	return network.Config["ipv4.address"], nil
}

func (c *sdkClient) OpenPortForward(ctx context.Context, name, address string, port int) (net.Conn, error) {
	type outcome struct {
		conn net.Conn
		err  error
	}
	dialCtx, cancel := context.WithTimeout(ctx, portForwardDialTimeout)
	defer cancel()
	result := make(chan outcome, 1)
	server := c.serverFor(dialCtx)
	// Incus v7.3 does not pass a context into its raw port-forward dial. Keep
	// Dorf's caller-visible wait bounded and close any connection returned
	// after cancellation; this deliberately does not claim the SDK's
	// underlying raw dial itself is cancellable.
	go func() {
		conn, err := server.GetInstancePortForwardConn(name, api.InstancePortForwardPost{Address: address, Port: port})
		if dialCtx.Err() != nil {
			if conn != nil {
				_ = conn.Close()
			}
			return
		}
		result <- outcome{conn: conn, err: err}
	}()
	select {
	case <-dialCtx.Done():
		c.Close()
		return nil, dialCtx.Err()
	case opened := <-result:
		if err := dialCtx.Err(); err != nil {
			if opened.conn != nil {
				_ = opened.conn.Close()
			}
			return nil, err
		}
		if opened.err != nil {
			return nil, fmt.Errorf("open Incus instance port forward: %w", opened.err)
		}
		return opened.conn, nil
	}
}

func cloneStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

var _ ClientFactory = SDKClientFactory{}
var _ Client = (*sdkClient)(nil)
