package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const (
	Harness    = "pi"
	routeKey   = "/root/.config/dorf/provider-route.key"
	modelsFile = "/root/.pi/agent/models.json"
)

type Agent struct {
	Sandbox provider.Sandbox
}

func (Agent) Name() string { return Harness }

func (a Agent) InstallRoute(ctx context.Context, owner provider.Ownership, baseURL, key, model string) error {
	config, err := json.Marshal(map[string]any{"providers": map[string]any{"dorf": map[string]any{
		"baseUrl": baseURL,
		"api":     "openai-responses",
		"apiKey":  "$DORF_PROVIDER_ROUTE_KEY",
		"models":  []map[string]any{{"id": model, "reasoning": true}},
	}}})
	if err != nil {
		return err
	}
	input := append(append(config, '\n'), []byte(key+"\n")...)
	script := "set -eu; umask 077; install -d -m 700 /root/.pi/agent /root/.config/dorf; IFS= read -r config; printf '%s\\n' \"$config\" > " + modelsFile + "; IFS= read -r key; printf '%s\\n' \"$key\" > " + routeKey
	result, err := a.Sandbox.Exec(ctx, owner, input, "bash", "-lc", script)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("install Pi scoped provider route: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (a Agent) RemoveRoute(ctx context.Context, owner provider.Ownership) error {
	result, err := a.Sandbox.Exec(ctx, owner, nil, "rm", "-f", routeKey, modelsFile)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("remove Pi scoped provider route: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}
