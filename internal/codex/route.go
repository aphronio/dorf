package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"

	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/pelletier/go-toml/v2"
)

const routeConfigPath = "/root/.codex/config.toml"
const routeKeyPath = "/root/.config/dorf/provider-route.key"
const routeBegin = "# BEGIN DORF MODEL ROUTE v1\n"
const routeEnd = "# END DORF MODEL ROUTE v1\n"

func routeProvider(baseURL string) map[string]any {
	return map[string]any{"name": "Dorf Provider Gateway", "base_url": baseURL,
		"env_key": "DORF_PROVIDER_ROUTE_KEY", "wire_api": "responses", "supports_websockets": true, "requires_openai_auth": false}
}

// routeClientConfig removes only our exact managed prefix (or the complete
// legacy file we used to generate). All remaining client bytes are retained.
func routeClientConfig(contents []byte) ([]byte, error) {
	client := contents
	if bytes.HasPrefix(contents, []byte(routeBegin)) {
		end := bytes.Index(contents, []byte(routeEnd))
		if end < 0 {
			return nil, errors.New("Codex managed route section is incomplete")
		}
		managed := contents[len(routeBegin):end]
		var config map[string]any
		if toml.Unmarshal(managed, &config) != nil {
			return nil, errors.New("invalid Codex managed route section")
		}
		url, ok := managedRouteURL(config)
		if !ok || !bytes.Equal(contents[:end+len(routeEnd)], managedRouteConfig(url)) {
			return nil, errors.New("Codex managed route section has changed")
		}
		client = contents[end+len(routeEnd):]
	} else {
		var config map[string]any
		if toml.Unmarshal(contents, &config) == nil {
			if url, ok := managedRouteURL(config); ok && string(contents) == legacyRouteConfig(url) {
				client = nil
			}
		}
	}
	return client, nil
}

func validateRouteClient(client []byte) error {
	var config map[string]any
	if toml.Unmarshal(client, &config) != nil {
		return errors.New("invalid client Codex configuration")
	}
	if _, exists := config["model_provider"]; exists {
		return errors.New("client Codex model_provider conflicts with Dorf route ownership")
	}
	if providers, ok := config["model_providers"].(map[string]any); ok {
		if _, exists := providers["dorf"]; exists {
			return errors.New("client Codex provider dorf conflicts with Dorf route ownership")
		}
	}
	return nil
}

func managedRouteURL(config map[string]any) (string, bool) {
	providers, _ := config["model_providers"].(map[string]any)
	route, _ := providers["dorf"].(map[string]any)
	url, ok := route["base_url"].(string)
	return url, ok
}

func managedRouteConfig(baseURL string) []byte {
	// Dotted inline assignment leaves the following client document in root scope.
	encoded, _ := toml.Marshal(routeProvider(baseURL))
	fields := strings.Split(strings.TrimSpace(string(encoded)), "\n")
	return []byte(routeBegin + "model_provider = \"dorf\"\nmodel_providers.dorf = { " + strings.Join(fields, ", ") + " }\n" + routeEnd)
}

func legacyRouteConfig(baseURL string) string {
	return fmt.Sprintf("model_provider = \"dorf\"\n\n[model_providers.dorf]\nname = \"Dorf Provider Gateway\"\nbase_url = %q\nenv_key = \"DORF_PROVIDER_ROUTE_KEY\"\nwire_api = \"responses\"\nsupports_websockets = true\nrequires_openai_auth = false\n", baseURL)
}

func (a Agent) InstallRoute(ctx context.Context, owner provider.Ownership, baseURL, key, _ string) error {
	previous, missing, err := a.readRouteConfig(ctx, owner)
	if err != nil {
		return err
	}
	client, err := routeClientConfig(previous)
	if err != nil {
		return err
	}
	if err := validateRouteClient(client); err != nil {
		return err
	}
	desired := append(managedRouteConfig(baseURL), client...)
	var config map[string]any
	if toml.Unmarshal(desired, &config) != nil {
		return errors.New("Codex route conflicts with client configuration structure")
	}
	if err := a.Sandbox.PutFile(ctx, owner, routeKeyPath, []byte(key+"\n")); err != nil {
		return err
	}
	return a.writeRouteConfig(ctx, owner, previous, desired, missing)
}

func (a Agent) RemoveRoute(ctx context.Context, owner provider.Ownership) error {
	// Local configuration damage must not prevent credential removal. The caller
	// revokes remote authority before invoking this adapter.
	result, err := a.Sandbox.Exec(ctx, owner, nil, "rm", "-f", routeKeyPath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return errors.New("remove Codex scoped route credential failed")
	}
	previous, missing, err := a.readRouteConfig(ctx, owner)
	if err != nil || missing {
		return err
	}
	client, err := routeClientConfig(previous)
	if err != nil {
		return err
	}
	if !bytes.Equal(previous, client) {
		var config map[string]any
		if toml.Unmarshal(client, &config) != nil {
			return errors.New("invalid client Codex configuration; credential removed, configuration retained")
		}
	}
	return a.writeRouteConfig(ctx, owner, previous, client, false)
}

func (a Agent) readRouteConfig(ctx context.Context, owner provider.Ownership) ([]byte, bool, error) {
	contents, err := a.Sandbox.ReadFile(ctx, owner, routeConfigPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	return contents, false, err
}

func (a Agent) writeRouteConfig(ctx context.Context, owner provider.Ownership, previous, desired []byte, missing bool) error {
	if !missing && bytes.Equal(previous, desired) {
		return nil
	}
	expected := fmt.Sprintf("%x", sha256.Sum256(previous))
	if missing {
		expected = "missing"
	}
	result, err := a.Sandbox.Exec(ctx, owner, desired, "bash", "-c", writeRouteConfigScript, "dorf-route-config", routeConfigPath, expected)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return errors.New("Codex route configuration write failed or configuration changed; reconcile before retry")
	}
	return nil
}

// The Job fence serializes Dorf writers. The comparison also detects edits since
// our read; privileged guest processes remain outside that fence.
const writeRouteConfigScript = `set -eu
umask 077
target=$1 expected=$2
parent=$(dirname -- "$target")
test "$(realpath -m -- "$parent")" = "$parent"
mkdir -p -- "$parent"
temporary=$(mktemp "$parent/.dorf-route.XXXXXXXX")
trap 'rm -f -- "$temporary"' EXIT
cat > "$temporary"
if test "$expected" = missing; then
  test ! -e "$target" && test ! -L "$target"
else
  test -f "$target" && test ! -L "$target"
  observed=$(sha256sum -- "$target")
  test "${observed%% *}" = "$expected"
fi
mv -f -- "$temporary" "$target"
`
