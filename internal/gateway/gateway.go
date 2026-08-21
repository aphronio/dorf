package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Route struct {
	ID             string `json:"id"`
	ConnectionName string `json:"connection_name"`
	Consumer       string `json:"consumer"`
	APIKey         string `json:"api_key"`
	BaseURL        string `json:"-"`
}

type connection struct {
	Name          string `json:"name"`
	Provider      string `json:"provider"`
	AuthMode      string `json:"auth_mode"`
	CredentialRef string `json:"credential_ref"`
}

type authority struct {
	GuardKey      string `json:"guard_key"`
	ManagementKey string `json:"management_key"`
}

type Gateway struct {
	StatePath      string
	PrivateBridge  string
	Client         *http.Client
	UpstreamClient *http.Client
}

func (g Gateway) BaseURL() (string, error) {
	origin, err := g.origin()
	if err != nil {
		return "", err
	}
	return origin + "/v1", nil
}

func (g Gateway) ReconcileCreate(ctx context.Context, connectionName, consumer, routeID string) (Route, error) {
	if strings.TrimSpace(routeID) == "" {
		return Route{}, fmt.Errorf("exact provider route creation requires a route ID")
	}
	var route Route
	err := g.lock(func() error {
		connection, err := g.requireConnection(connectionName)
		if err != nil {
			return err
		}
		if err := g.validateTransport(ctx, connection); err != nil {
			return err
		}
		routes, err := g.readRoutes()
		if err != nil {
			return err
		}
		for _, existing := range routes {
			if existing.ID == routeID && existing.Consumer != consumer {
				return fmt.Errorf("provider route ID is owned by a different consumer")
			}
			if existing.Consumer == consumer {
				if existing.ConnectionName != connectionName || existing.ID != routeID {
					return fmt.Errorf("provider consumer is bound to a different stable route")
				}
				route = existing
				return g.activate(ctx, routes)
			}
		}
		key, err := randomKey()
		if err != nil {
			return err
		}
		route = Route{ID: routeID, ConnectionName: connectionName, Consumer: consumer, APIKey: key}
		routes = append(routes, route)
		if err := g.writeRoutes(routes); err != nil {
			return err
		}
		return g.activate(ctx, routes)
	})
	if err != nil {
		return Route{}, err
	}
	origin, err := g.origin()
	if err != nil {
		return Route{}, err
	}
	route.BaseURL = origin + "/v1"
	return route, nil
}

// RevokeExact removes only the recorded consumer/route pair. An absent
// consumer reconciles a prior success-before-record loss; a changed identity
// fails closed without touching any route.
func (g Gateway) RevokeExact(ctx context.Context, consumer, expectedRouteID string) error {
	if strings.TrimSpace(consumer) == "" || strings.TrimSpace(expectedRouteID) == "" {
		return fmt.Errorf("exact provider route revocation requires consumer and route ID")
	}
	return g.lock(func() error {
		routes, err := g.readRoutes()
		if err != nil {
			return err
		}
		index := -1
		for i, route := range routes {
			if route.Consumer == consumer {
				if route.ID != expectedRouteID {
					return fmt.Errorf("provider consumer %q is bound to route %s, not recorded route %s", consumer, route.ID, expectedRouteID)
				}
				index = i
			}
			if route.ID == expectedRouteID && route.Consumer != consumer {
				return fmt.Errorf("recorded provider route %s belongs to consumer %q, not %q", expectedRouteID, route.Consumer, consumer)
			}
		}
		if index < 0 {
			return g.activate(ctx, routes)
		}
		remaining := append(routes[:index:index], routes[index+1:]...)
		if err := g.writeRoutes(remaining); err != nil {
			return err
		}
		return g.activate(ctx, remaining)
	})
}

func (g Gateway) Route(ctx context.Context, consumer string) (Route, bool, error) {
	var found Route
	err := g.lock(func() error {
		routes, err := g.readRoutes()
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Consumer == consumer {
				found = route
				break
			}
		}
		return nil
	})
	if err != nil || found.ID == "" {
		return Route{}, false, err
	}
	origin, err := g.origin()
	if err != nil {
		return Route{}, false, err
	}
	found.BaseURL = origin + "/v1"
	connection, err := g.requireConnection(found.ConnectionName)
	if err != nil {
		return Route{}, false, err
	}
	if err := g.validateTransport(ctx, connection); err != nil {
		return Route{}, false, err
	}
	return found, true, nil
}

func (g Gateway) Check(ctx context.Context, connectionName string) error {
	connection, err := g.requireConnection(connectionName)
	if err != nil {
		return err
	}
	if err := g.validateTransport(ctx, connection); err != nil {
		return err
	}
	auth, err := g.readAuthority()
	if err != nil {
		return err
	}
	origin, err := g.origin()
	if err != nil {
		return err
	}
	return g.checkModels(ctx, origin+"/v1", auth.GuardKey, "provider gateway")
}

// CheckRemote observes whether the exact deployment-owned HTTPS route reaches
// a protected Gateway API. It deliberately sends no credential and succeeds
// only when anonymous access is rejected. It does not create a consumer route,
// restart the broker, or otherwise mutate Gateway state.
func (g Gateway) CheckRemote(ctx context.Context, baseURL string) error {
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("remote provider gateway route URL is empty")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return err
	}
	response, err := g.client().Do(request)
	if err != nil {
		return fmt.Errorf("remote provider gateway route is unavailable: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return nil
	case http.StatusOK:
		return fmt.Errorf("remote provider gateway route accepted an unauthenticated request")
	default:
		return fmt.Errorf("remote provider gateway route returned HTTP %d, want the Gateway's unauthenticated HTTP 401", response.StatusCode)
	}
}

func (g Gateway) checkModels(ctx context.Context, baseURL, apiKey, noun string) error {
	if baseURL == "" {
		return fmt.Errorf("%s URL is empty", noun)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := g.client().Do(request)
	if err != nil {
		return fmt.Errorf("%s is unavailable: %w", noun, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s readiness returned HTTP %d", noun, response.StatusCode)
	}
	return nil
}

func (g Gateway) requireConnection(name string) (connection, error) {
	var records []connection
	if err := readJSON(filepath.Join(g.StatePath, "connections.json"), &records); err != nil {
		return connection{}, fmt.Errorf("provider connections are unreadable: %w", err)
	}
	selectedCategory := ""
	for _, record := range records {
		if record.Name == name {
			selectedCategory = "unprefixed"
			if record.Provider == "deepseek" {
				selectedCategory = "deepseek"
			}
		}
	}
	if selectedCategory == "" {
		return connection{}, fmt.Errorf("provider connection %q was not found", name)
	}
	categoryCount := 0
	for _, record := range records {
		category := "unprefixed"
		if record.Provider == "deepseek" {
			category = "deepseek"
		}
		if category == selectedCategory {
			categoryCount++
		}
	}
	if categoryCount > 1 {
		return connection{}, fmt.Errorf("provider selection is ambiguous for connection %q", name)
	}
	for _, record := range records {
		if record.Name != name {
			continue
		}
		root := "auth"
		validCredential := regexp.MustCompile(`^codex-[^/\\]+\.json$`).MatchString(record.CredentialRef) && record.Provider == "chatgpt" && record.AuthMode == "subscription"
		if record.AuthMode == "api_key" {
			root = "credentials"
			validCredential = regexp.MustCompile(`^(openai|deepseek)-[0-9a-f]{16}\.key$`).MatchString(record.CredentialRef) && (record.Provider == "openai" || record.Provider == "deepseek")
		}
		if !validCredential || filepath.Base(record.CredentialRef) != record.CredentialRef {
			return connection{}, fmt.Errorf("provider connection %q has invalid credential metadata", name)
		}
		info, err := os.Lstat(filepath.Join(g.StatePath, root, record.CredentialRef))
		if err != nil || !info.Mode().IsRegular() {
			return connection{}, fmt.Errorf("provider connection %q needs authentication", name)
		}
		return record, nil
	}
	return connection{}, fmt.Errorf("provider connection %q needs authentication", name)
}

func (g Gateway) validateTransport(ctx context.Context, record connection) error {
	switch {
	case record.Provider == "chatgpt" && record.AuthMode == "subscription":
		enabled, err := g.chatGPTWebSocketsEnabled(ctx, record)
		if err != nil {
			return err
		}
		if !enabled {
			return fmt.Errorf("provider connection %q does not support required Responses WebSockets", record.Name)
		}
		return nil
	case record.Provider == "openai" && record.AuthMode == "api_key":
		return nil
	case record.Provider == "deepseek" && record.AuthMode == "api_key":
		return nil
	default:
		return fmt.Errorf("unsupported provider authentication: %s/%s", record.Provider, record.AuthMode)
	}
}

func (g Gateway) chatGPTWebSocketsEnabled(ctx context.Context, record connection) (bool, error) {
	auth, err := g.readAuthority()
	if err != nil {
		return false, err
	}
	origin, err := g.origin()
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/v0/management/auth-files", nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+auth.ManagementKey)
	response, err := g.client().Do(request)
	if err != nil {
		return false, fmt.Errorf("provider connection capability is unavailable: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		return false, fmt.Errorf("provider connection capability returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Files []struct {
			Name       string `json:"name"`
			Provider   string `json:"provider"`
			WebSockets bool   `json:"websockets"`
		} `json:"files"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("provider connection capability is unreadable")
	}
	for _, entry := range payload.Files {
		if entry.Name == record.CredentialRef && entry.Provider == "codex" {
			return entry.WebSockets, nil
		}
	}
	return false, fmt.Errorf("provider connection capability was not found")
}

func (g Gateway) activate(ctx context.Context, routes []Route) error {
	auth, err := g.readAuthority()
	if err != nil {
		return err
	}
	keys := []string{auth.GuardKey}
	for _, route := range routes {
		keys = append(keys, route.APIKey)
	}
	payload, _ := json.Marshal(keys)
	origin, err := g.origin()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, origin+"/v0/management/api-keys", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+auth.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("provider gateway route control is unavailable: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider gateway rejected route reconciliation with HTTP %d", response.StatusCode)
	}
	return nil
}

func (g Gateway) origin() (string, error) {
	raw, err := os.ReadFile(filepath.Join(g.StatePath, "broker.yaml"))
	if err != nil {
		return "", fmt.Errorf("provider gateway config is unreadable: %w", err)
	}
	host, port := "", 0
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch key {
		case "host":
			_ = json.Unmarshal([]byte(strings.TrimSpace(value)), &host)
		case "port":
			port, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}
	if host == "" || port < 1 || port > 65535 {
		return "", fmt.Errorf("provider gateway host/port is invalid")
	}
	return fmt.Sprintf("http://%s:%d", host, port), nil
}

func (g Gateway) readRoutes() ([]Route, error) {
	var routes []Route
	err := readJSON(filepath.Join(g.StatePath, "routes.json"), &routes)
	if os.IsNotExist(err) {
		return []Route{}, nil
	}
	if err != nil {
		return nil, err
	}
	ids, consumers := map[string]bool{}, map[string]bool{}
	for _, route := range routes {
		if route.ID == "" || route.ConnectionName == "" || route.Consumer == "" || route.APIKey == "" || ids[route.ID] || consumers[route.Consumer] {
			return nil, fmt.Errorf("provider gateway route state is invalid")
		}
		ids[route.ID], consumers[route.Consumer] = true, true
	}
	return routes, nil
}

func (g Gateway) writeRoutes(routes []Route) error {
	path := filepath.Join(g.StatePath, "routes.json")
	data, err := json.MarshalIndent(routes, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(g.StatePath, ".routes-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	directory, err := os.Open(g.StatePath)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (g Gateway) readAuthority() (authority, error) {
	var auth authority
	if err := readJSON(filepath.Join(g.StatePath, "authority.json"), &auth); err != nil {
		return authority{}, fmt.Errorf("provider gateway authority is unreadable: %w", err)
	}
	if auth.GuardKey == "" || auth.ManagementKey == "" {
		return authority{}, fmt.Errorf("provider gateway authority is incomplete")
	}
	return auth, nil
}

func (g Gateway) lock(fn func() error) error {
	file, err := os.OpenFile(filepath.Join(g.StatePath, "broker.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return fn()
}

func (g Gateway) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Timeout: 5 * time.Second, Transport: transport}
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func randomKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "agw_" + hex.EncodeToString(raw), nil
}
