package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	StatePath string
	Client    *http.Client
}

func (g Gateway) BaseURL() (string, error) {
	origin, err := g.origin()
	if err != nil {
		return "", err
	}
	return origin + "/v1", nil
}

func (g Gateway) ReconcileCreate(ctx context.Context, connectionName, consumer, actionID string) (Route, error) {
	var route Route
	err := g.lock(func() error {
		if err := g.requireConnection(connectionName); err != nil {
			return err
		}
		routes, err := g.readRoutes()
		if err != nil {
			return err
		}
		for _, existing := range routes {
			if existing.Consumer == consumer {
				if existing.ConnectionName != connectionName || existing.ID != routeID(actionID) {
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
		route = Route{ID: routeID(actionID), ConnectionName: connectionName, Consumer: consumer, APIKey: key}
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

func (g Gateway) Revoke(ctx context.Context, consumer string) (string, error) {
	removedID := "absent"
	err := g.lock(func() error {
		routes, err := g.readRoutes()
		if err != nil {
			return err
		}
		remaining := routes[:0]
		for _, route := range routes {
			if route.Consumer == consumer {
				removedID = route.ID
				continue
			}
			remaining = append(remaining, route)
		}
		if len(remaining) != len(routes) {
			if err := g.writeRoutes(remaining); err != nil {
				return err
			}
		}
		return g.activate(ctx, remaining)
	})
	return removedID, err
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
	return found, true, nil
}

func (g Gateway) Check(ctx context.Context, connectionName string) error {
	if err := g.requireConnection(connectionName); err != nil {
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
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/v1/models", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+auth.GuardKey)
	response, err := g.client().Do(request)
	if err != nil {
		return fmt.Errorf("provider gateway is unavailable: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("provider gateway readiness returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (g Gateway) requireConnection(name string) error {
	var records []connection
	if err := readJSON(filepath.Join(g.StatePath, "connections.json"), &records); err != nil {
		return fmt.Errorf("provider connections are unreadable: %w", err)
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
		return fmt.Errorf("provider connection %q was not found", name)
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
		return fmt.Errorf("provider selection is ambiguous for connection %q", name)
	}
	for _, record := range records {
		if record.Name != name {
			continue
		}
		root := "auth"
		validCredential := regexp.MustCompile(`^codex-dorf-[0-9a-f]{16}\.json$`).MatchString(record.CredentialRef) && record.Provider == "chatgpt" && record.AuthMode == "subscription"
		if record.AuthMode == "api_key" {
			root = "credentials"
			validCredential = regexp.MustCompile(`^(openai|deepseek)-[0-9a-f]{16}\.key$`).MatchString(record.CredentialRef) && (record.Provider == "openai" || record.Provider == "deepseek")
		}
		if !validCredential || filepath.Base(record.CredentialRef) != record.CredentialRef {
			return fmt.Errorf("provider connection %q has invalid credential metadata", name)
		}
		info, err := os.Lstat(filepath.Join(g.StatePath, root, record.CredentialRef))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("provider connection %q needs authentication", name)
		}
		return nil
	}
	return fmt.Errorf("provider connection %q needs authentication", name)
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

func routeID(actionID string) string {
	sum := sha256.Sum256([]byte(actionID))
	return "route-" + hex.EncodeToString(sum[:])[:16]
}

func randomKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "agw_" + hex.EncodeToString(raw), nil
}
