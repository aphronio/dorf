// Package e2b implements the E2B lifecycle and process primitives earned by
// Dorf's incremental second-Sandbox-provider proofs.
package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const (
	DefaultAPIURL = "https://api.e2b.app"
	listPageSize  = 100
)

const (
	metadataOwner          = "dorf.owner"
	metadataJob            = "dorf.job"
	metadataSandbox        = "dorf.sandbox"
	metadataOwnershipNonce = "dorf.ownership_nonce"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	APIURL     string
	APIKey     string
	HTTPClient HTTPClient
}

// Ownership is Dorf's durable identity for a provider Sandbox. Provider IDs
// are opaque locators and are deliberately not part of this identity.
type Ownership struct {
	JobID          string
	SandboxID      string
	OwnershipNonce string
}

type CreateRequest struct {
	Template         string
	Timeout          time.Duration
	Owner            Ownership
	AllowedHostnames []string
	AllowInternet    bool
}

type Sandbox struct {
	ProviderID  string
	TemplateID  string
	EnvdVersion string
	State       string
	Metadata    map[string]string
	StartedAt   time.Time
	EndAt       time.Time
}

// EnvdConnection contains the short-lived, Sandbox-scoped material needed to
// reach envd. The access token is intentionally private so it cannot be
// serialized or logged by consumers.
type EnvdConnection struct {
	ProviderID  string
	Domain      string
	Version     string
	accessToken string
}

func (c EnvdConnection) String() string {
	return fmt.Sprintf("E2B envd connection %s (%s, envd %s, scoped token redacted)", c.ProviderID, c.Domain, c.Version)
}

func (c EnvdConnection) GoString() string { return c.String() }

type APIError struct {
	StatusCode int
	Code       int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("E2B API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("E2B API returned HTTP %d: %s", e.StatusCode, e.Message)
}

type OwnershipError = provider.OwnershipError

func ownershipErrorf(format string, args ...any) error {
	return provider.OwnershipErrorf(format, args...)
}

// Check proves that the configured project credential can reach E2B's
// Sandbox inventory without creating or changing a provider resource.
func (c Client) Check(ctx context.Context) error {
	query := url.Values{}
	query.Set("limit", "1")
	query.Set("state", "running,paused")
	var sandboxes []listedSandbox
	_, err := c.doJSONWithHeaders(ctx, http.MethodGet, "/v2/sandboxes", query, nil, http.StatusOK, &sandboxes)
	return err
}

func (c Client) Create(ctx context.Context, request CreateRequest) (Sandbox, error) {
	if err := validateOwnership(request.Owner); err != nil {
		return Sandbox{}, err
	}
	if strings.TrimSpace(request.Template) == "" {
		return Sandbox{}, fmt.Errorf("E2B Sandbox requires a pinned template reference")
	}
	if request.Timeout <= 0 || request.Timeout%time.Second != 0 {
		return Sandbox{}, fmt.Errorf("E2B Sandbox timeout must be a positive whole number of seconds")
	}
	body := struct {
		TemplateID          string            `json:"templateID"`
		Timeout             int64             `json:"timeout"`
		Secure              bool              `json:"secure"`
		Metadata            map[string]string `json:"metadata"`
		AllowInternetAccess bool              `json:"allow_internet_access"`
		Network             struct {
			AllowPublicTraffic bool     `json:"allowPublicTraffic"`
			AllowOut           []string `json:"allowOut,omitempty"`
			DenyOut            []string `json:"denyOut,omitempty"`
		} `json:"network"`
		AutoPause  bool `json:"autoPause"`
		AutoResume struct {
			Enabled bool `json:"enabled"`
		} `json:"autoResume"`
	}{
		TemplateID:          request.Template,
		Timeout:             int64(request.Timeout / time.Second),
		Secure:              true,
		Metadata:            request.Owner.metadata(),
		AllowInternetAccess: request.AllowInternet,
	}
	if !request.AllowInternet {
		body.Network.AllowOut = append([]string(nil), request.AllowedHostnames...)
	}
	if !request.AllowInternet && len(body.Network.AllowOut) > 0 {
		body.Network.DenyOut = []string{"0.0.0.0/0"}
	}
	var response createResponse
	if err := c.doJSON(ctx, http.MethodPost, "/sandboxes", nil, body, http.StatusCreated, &response); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && missingTemplate(apiErr) {
			return Sandbox{}, provider.ArtifactUnavailableErrorf("E2B template %q is unavailable: %v", request.Template, err)
		}
		return Sandbox{}, err
	}
	if response.SandboxID == "" {
		return Sandbox{}, fmt.Errorf("E2B create response omitted sandboxID")
	}
	return response.sandbox(), nil
}

// FindOwned exhaustively searches both running and paused E2B Sandboxes. It
// never creates a resource, which makes it safe after an ambiguous create.
func (c Client) FindOwned(ctx context.Context, owner Ownership) (*Sandbox, error) {
	if err := validateOwnership(owner); err != nil {
		return nil, err
	}
	metadata := url.Values{}
	for key, value := range owner.metadata() {
		metadata.Set(key, value)
	}
	var matches []Sandbox
	next := ""
	seen := map[string]bool{}
	for {
		query := url.Values{}
		query.Set("limit", strconv.Itoa(listPageSize))
		query.Set("metadata", metadata.Encode())
		query.Set("state", "running,paused")
		if next != "" {
			if seen[next] {
				return nil, fmt.Errorf("E2B list returned a repeated pagination token")
			}
			seen[next] = true
			query.Set("nextToken", next)
		}
		var page []listedSandbox
		headers, err := c.doJSONWithHeaders(ctx, http.MethodGet, "/v2/sandboxes", query, nil, http.StatusOK, &page)
		if err != nil {
			return nil, err
		}
		for _, candidate := range page {
			sandbox := candidate.sandbox()
			if owner.matches(sandbox.Metadata) {
				matches = append(matches, sandbox)
			}
		}
		next = strings.TrimSpace(headers.Get("X-Next-Token"))
		if next == "" {
			break
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) != 1 {
		return nil, ownershipErrorf("E2B Sandbox ownership is ambiguous: found %d exact matches", len(matches))
	}
	return &matches[0], nil
}

// InspectOwned re-reads the provider resource and verifies that its metadata
// still belongs to the expected durable Sandbox.
func (c Client) InspectOwned(ctx context.Context, providerID string, owner Ownership) (Sandbox, error) {
	if err := validateOwnership(owner); err != nil {
		return Sandbox{}, err
	}
	if strings.TrimSpace(providerID) == "" {
		return Sandbox{}, fmt.Errorf("E2B provider Sandbox ID is required")
	}
	var detail detailSandbox
	err := c.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(providerID), nil, nil, http.StatusOK, &detail)
	if err != nil {
		return Sandbox{}, err
	}
	sandbox := detail.sandbox()
	if sandbox.ProviderID != providerID || !owner.matches(sandbox.Metadata) {
		return Sandbox{}, ownershipErrorf("E2B Sandbox metadata does not match its durable owner")
	}
	return sandbox, nil
}

// ConnectEnvd extends the Sandbox lifetime and returns fresh, scoped envd
// connection material. It does not expose the team API key to the Sandbox.
func (c Client) ConnectEnvd(ctx context.Context, providerID string, timeout time.Duration) (EnvdConnection, error) {
	response, err := c.connect(ctx, providerID, timeout)
	if err != nil {
		return EnvdConnection{}, err
	}
	if response.EnvdVersion == "" || response.EnvdAccessToken == "" {
		return EnvdConnection{}, fmt.Errorf("E2B connect response omitted required scoped envd material")
	}
	domain := strings.TrimSpace(response.Domain)
	if domain == "" {
		domain = "e2b.app"
	}
	return EnvdConnection{ProviderID: providerID, Domain: domain, Version: response.EnvdVersion, accessToken: response.EnvdAccessToken}, nil
}

func (c Client) connect(ctx context.Context, providerID string, timeout time.Duration) (connectionResponse, error) {
	if strings.TrimSpace(providerID) == "" {
		return connectionResponse{}, fmt.Errorf("E2B provider Sandbox ID is required")
	}
	if timeout <= 0 || timeout%time.Second != 0 {
		return connectionResponse{}, fmt.Errorf("E2B Sandbox timeout must be a positive whole number of seconds")
	}
	var response connectionResponse
	body := struct {
		Timeout int64 `json:"timeout"`
	}{Timeout: int64(timeout / time.Second)}
	if err := c.doJSONOneOf(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(providerID)+"/connect", nil, body, []int{http.StatusOK, http.StatusCreated}, &response); err != nil {
		return connectionResponse{}, err
	}
	if response.SandboxID != providerID {
		return connectionResponse{}, fmt.Errorf("E2B connect response returned a foreign Sandbox ID")
	}
	return response, nil
}

// DeleteOwned refuses to delete a resource until exact ownership is attested.
// An already absent provider resource is settled successfully.
func (c Client) DeleteOwned(ctx context.Context, providerID string, owner Ownership) error {
	_, err := c.InspectOwned(ctx, providerID, owner)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			owned, lookupErr := c.FindOwned(ctx, owner)
			if lookupErr != nil {
				return lookupErr
			}
			if owned == nil {
				return nil
			}
			return ownershipErrorf("expected E2B Sandbox is absent but owned resource %s remains", owned.ProviderID)
		}
		return err
	}
	err = c.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(providerID), nil, nil, http.StatusNoContent, nil)
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		owned, lookupErr := c.FindOwned(ctx, owner)
		if lookupErr != nil {
			return lookupErr
		}
		if owned == nil {
			return nil
		}
		return ownershipErrorf("E2B delete returned absent but owned resource %s remains", owned.ProviderID)
	}
	return err
}

func (c Client) doJSON(ctx context.Context, method, path string, query url.Values, input any, wantStatus int, output any) error {
	_, err := c.doJSONWithHeaders(ctx, method, path, query, input, wantStatus, output)
	return err
}

func (c Client) doJSONWithHeaders(ctx context.Context, method, path string, query url.Values, input any, wantStatus int, output any) (http.Header, error) {
	return c.doJSONStatuses(ctx, method, path, query, input, []int{wantStatus}, output)
}

func (c Client) doJSONOneOf(ctx context.Context, method, path string, query url.Values, input any, wantStatuses []int, output any) error {
	_, err := c.doJSONStatuses(ctx, method, path, query, input, wantStatuses, output)
	return err
}

func (c Client) doJSONStatuses(ctx context.Context, method, path string, query url.Values, input any, wantStatuses []int, output any) (http.Header, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return nil, fmt.Errorf("E2B API key is required")
	}
	base := strings.TrimRight(strings.TrimSpace(c.APIURL), "/")
	if base == "" {
		base = DefaultAPIURL
	}
	endpoint, err := url.Parse(base + path)
	if err != nil {
		return nil, fmt.Errorf("build E2B API URL: %w", err)
	}
	endpoint.RawQuery = query.Encode()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode E2B request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build E2B request: %w", err)
	}
	request.Header.Set("X-API-Key", c.APIKey)
	request.Header.Set("Accept", "application/json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("E2B %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	if !containsStatus(wantStatuses, response.StatusCode) {
		return nil, decodeAPIError(response, c.APIKey)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return response.Header.Clone(), err
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(output); err != nil {
		return nil, fmt.Errorf("decode E2B %s %s response: %w", method, path, err)
	}
	return response.Header.Clone(), nil
}

func containsStatus(statuses []int, status int) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func decodeAPIError(response *http.Response, apiKey string) error {
	var payload struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&payload)
	if apiKey = strings.TrimSpace(apiKey); apiKey != "" {
		payload.Message = strings.ReplaceAll(payload.Message, apiKey, "[redacted]")
	}
	return &APIError{StatusCode: response.StatusCode, Code: payload.Code, Message: payload.Message}
}

func missingTemplate(apiErr *APIError) bool {
	if apiErr == nil || apiErr.StatusCode != http.StatusNotFound {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(apiErr.Message))
	return strings.Contains(message, "template") &&
		(strings.Contains(message, "not found") || strings.Contains(message, "does not exist"))
}

func validateOwnership(owner Ownership) error {
	if strings.TrimSpace(owner.JobID) == "" || strings.TrimSpace(owner.SandboxID) == "" || len(owner.OwnershipNonce) != 64 {
		return fmt.Errorf("E2B Sandbox requires complete host-owned identity metadata")
	}
	return nil
}

func (o Ownership) metadata() map[string]string {
	return map[string]string{
		metadataOwner:          "sandbox",
		metadataJob:            o.JobID,
		metadataSandbox:        o.SandboxID,
		metadataOwnershipNonce: o.OwnershipNonce,
	}
}

func (o Ownership) matches(metadata map[string]string) bool {
	want := o.metadata()
	for key, value := range want {
		if metadata[key] != value {
			return false
		}
	}
	return true
}

type createResponse struct {
	SandboxID   string `json:"sandboxID"`
	TemplateID  string `json:"templateID"`
	EnvdVersion string `json:"envdVersion"`
}

type connectionResponse struct {
	SandboxID          string `json:"sandboxID"`
	Domain             string `json:"domain"`
	EnvdVersion        string `json:"envdVersion"`
	EnvdAccessToken    string `json:"envdAccessToken"`
	TrafficAccessToken string `json:"trafficAccessToken"`
}

func (s createResponse) sandbox() Sandbox {
	return Sandbox{ProviderID: s.SandboxID, TemplateID: s.TemplateID, EnvdVersion: s.EnvdVersion, State: "running"}
}

type listedSandbox struct {
	SandboxID   string            `json:"sandboxID"`
	TemplateID  string            `json:"templateID"`
	EnvdVersion string            `json:"envdVersion"`
	State       string            `json:"state"`
	Metadata    map[string]string `json:"metadata"`
	StartedAt   time.Time         `json:"startedAt"`
	EndAt       time.Time         `json:"endAt"`
}

func (s listedSandbox) sandbox() Sandbox {
	return Sandbox{ProviderID: s.SandboxID, TemplateID: s.TemplateID, EnvdVersion: s.EnvdVersion, State: s.State, Metadata: s.Metadata, StartedAt: s.StartedAt, EndAt: s.EndAt}
}

type detailSandbox listedSandbox

func (s detailSandbox) sandbox() Sandbox { return listedSandbox(s).sandbox() }
