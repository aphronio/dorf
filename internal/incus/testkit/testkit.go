// Package testkit adapts existing command-script test doubles to Incus's
// narrow SDK-facing Client interface. It is imported only by tests; Dorf's
// runtime has no command-backed Incus path.
package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/aphronio/dorf/internal/incus"
)

type Runner interface {
	Run(context.Context, string, []byte, ...string) (incus.Result, error)
}

func Sandbox(runner Runner, config incus.Config) incus.Sandbox {
	return incus.Sandbox{Config: config, ClientFactory: factory{runner: runner}}
}

func OwnedSandbox(runner Runner, config incus.Config, owner incus.OwnershipMetadata) incus.Sandbox {
	return incus.Sandbox{Config: config, ClientFactory: factory{runner: runner, owner: &owner}}
}

type factory struct {
	runner Runner
	owner  *incus.OwnershipMetadata
}

func (f factory) Open(context.Context, incus.ConnectionConfig) (incus.Client, error) {
	if f.runner == nil {
		return nil, errors.New("test Incus runner is required")
	}
	return client{runner: f.runner, owner: f.owner}, nil
}

type client struct {
	runner Runner
	owner  *incus.OwnershipMetadata
}

func (c client) run(ctx context.Context, input []byte, args ...string) (incus.Result, error) {
	return c.runner.Run(ctx, "incus", input, args...)
}

func (c client) Instances(ctx context.Context) ([]incus.Instance, error) {
	if c.owner != nil {
		return []incus.Instance{{Name: c.owner.SandboxID, Running: true, Config: map[string]string{
			"user.dorf.owner": "sandbox", "user.dorf.job": c.owner.JobID, "user.dorf.sandbox": c.owner.SandboxID,
			"user.dorf.ownership_nonce": c.owner.OwnershipNonce,
		}}}, nil
	}
	result, err := c.run(ctx, nil, "list", "--format=json")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("test Incus list: %s", strings.TrimSpace(result.Stderr))
	}
	var raw []struct {
		Name   string            `json:"name"`
		Status string            `json:"status"`
		Config map[string]string `json:"config"`
	}
	if strings.TrimSpace(result.Stdout) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(result.Stdout), &raw); err != nil {
		return nil, err
	}
	instances := make([]incus.Instance, 0, len(raw))
	for _, item := range raw {
		instances = append(instances, incus.Instance{Name: item.Name, Config: item.Config, Running: strings.EqualFold(item.Status, "running")})
	}
	return instances, nil
}

func (c client) Instance(ctx context.Context, name string) (incus.Instance, error) {
	result, err := c.run(ctx, nil, "info", name)
	if err != nil {
		return incus.Instance{}, err
	}
	if result.ExitCode != 0 && strings.Contains(strings.ToLower(result.Stdout+result.Stderr), "not found") {
		return incus.Instance{}, incus.ErrNotFound
	}
	return incus.Instance{Name: name, Running: true}, nil
}

func (c client) CreateInstance(ctx context.Context, request incus.CreateInstanceRequest) error {
	args := []string{"init", request.Image, request.Name, "--vm", "--network", request.Network, "-d", "root,size=" + request.DiskSize}
	for key, value := range request.Config {
		args = append(args, "-c", key+"="+value)
	}
	result, err := c.run(ctx, nil, args...)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("test Incus create: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (c client) PatchInstanceConfig(ctx context.Context, name string, _ map[string]string, updates map[string]string) error {
	for key, value := range updates {
		result, err := c.run(ctx, nil, "config", "set", name, key, value)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("test Incus config: %s", strings.TrimSpace(result.Stderr))
		}
	}
	return nil
}

func (c client) StartInstance(ctx context.Context, name string) error {
	result, err := c.run(ctx, nil, "start", name)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 && !strings.Contains(strings.ToLower(result.Stdout+result.Stderr), "already running") {
		return fmt.Errorf("test Incus start: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (c client) DeleteInstance(ctx context.Context, name string, _ map[string]string) error {
	result, err := c.run(ctx, nil, "delete", name, "--force")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("test Incus delete: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (c client) Exec(ctx context.Context, name string, input []byte, args ...string) (incus.Result, error) {
	incusArgs := []string{"exec", name, "--"}
	incusArgs = append(incusArgs, args...)
	return c.run(ctx, input, incusArgs...)
}

func (c client) NetworkIPv4(ctx context.Context, name string) (string, error) {
	result, err := c.run(ctx, nil, "network", "get", name, "ipv4.address")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("test Incus network: %s", strings.TrimSpace(result.Stderr))
	}
	return result.Stdout, nil
}

func (client) OpenPortForward(context.Context, string, string, int) (net.Conn, error) {
	return nil, errors.New("test Incus port forward is not configured")
}

func (client) Close() {}

var _ incus.ClientFactory = factory{}
var _ incus.Client = client{}
