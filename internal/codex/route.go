package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const routeKeyPath = "/root/.config/dorf/provider-route.key"
const routeOptionsPath = "/root/.config/dorf/codex-route.options"

// routeOptions is one native -c argument, never shell source. JSON string
// escaping is also valid in a TOML basic string and keeps the file single-line.
func routeOptions(baseURL string) []byte {
	url, _ := json.Marshal(baseURL)
	return fmt.Appendf(nil, "model_providers.dorf={name=\"Dorf Provider Gateway\",base_url=%s,env_key=\"DORF_PROVIDER_ROUTE_KEY\",wire_api=\"responses\",requires_openai_auth=false,supports_websockets=true}\n", url)
}

func (a Agent) InstallRoute(ctx context.Context, owner provider.Ownership, baseURL, key, _ string) error {
	if err := a.Sandbox.PutFile(ctx, owner, routeKeyPath, []byte(key+"\n")); err != nil {
		return err
	}
	return a.Sandbox.PutFile(ctx, owner, routeOptionsPath, routeOptions(baseURL))
}

func (a Agent) RemoveRoute(ctx context.Context, owner provider.Ownership) error {
	// The caller revokes remote authority first. Never edit native client config,
	// including legacy Dorf-written settings: without the key they grant no access.
	result, err := a.Sandbox.Exec(ctx, owner, nil, "rm", "-f", routeKeyPath, routeOptionsPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return errors.New("remove Codex scoped route files failed")
	}
	return nil
}

// Older workspaces have the provider in native config but no options file.
// Preserve that launch path until their next route installation. Reading the
// options as one quoted argument avoids evaluating guest file contents as shell.
const loadRouteOptions = `route_options=(); if test -e ` + routeOptionsPath + `; then IFS= read -r route_option < ` + routeOptionsPath + `; test -n "$route_option"; route_options=(-c 'model_provider="dorf"' -c "$route_option"); fi; `
