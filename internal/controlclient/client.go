// Package controlclient is the small typed HTTP client for Dorf's first remote
// direct-Job API slice.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/controlapi"
)

// A valid 1 MiB goal can occupy more than 6 MiB after JSON escaping.
const maxResponseBytes = 8 << 20

// Client addresses one Deployment and presents one opaque client credential.
type Client struct {
	base       *url.URL
	credential string
	http       *http.Client
}

// ProblemError retains the stable Problem Details contract without including
// server prose or structured details in its printable error text.
type ProblemError struct {
	Problem controlapi.Problem
}

func (e *ProblemError) Error() string {
	if e.Problem.Code != "" {
		return fmt.Sprintf("Dorf API request failed with HTTP %d (%s)", e.Problem.Status, e.Problem.Code)
	}
	return fmt.Sprintf("Dorf API request failed with HTTP %d", e.Problem.Status)
}

// New constructs a client. A nil transport uses http.DefaultTransport.
func New(deploymentURL, credential string, transport http.RoundTripper) (*Client, error) {
	normalized, err := clientconfig.NormalizeDeploymentURL(deploymentURL)
	if err != nil {
		return nil, err
	}
	if credential == "" {
		return nil, fmt.Errorf("Dorf client credential is empty")
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("parse Deployment URL")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{
		base:       base,
		credential: credential,
		http: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Discover returns Deployment and capability metadata without authentication.
func (c *Client) Discover(ctx context.Context) (controlapi.Discovery, error) {
	var response controlapi.Discovery
	err := c.do(ctx, http.MethodGet, []string{"v1"}, nil, false, "", &response)
	return response, err
}

// RedeemEnrollment registers this client credential. Reusing the same client,
// enrollment code, and name safely replays a committed redemption.
func (c *Client) RedeemEnrollment(ctx context.Context, enrollmentCode, clientName string) (controlapi.Identity, error) {
	request := controlapi.RedeemRequest{
		EnrollmentCode: enrollmentCode,
		ClientName:     clientName,
		Credential:     c.credential,
	}
	var response controlapi.Identity
	err := c.do(ctx, http.MethodPost, []string{"v1", "auth", "enrollments", "redeem"}, request, false, "", &response)
	return response, err
}

// Me returns the effective authenticated Principal and Client.
func (c *Client) Me(ctx context.Context) (controlapi.Identity, error) {
	var response controlapi.Identity
	err := c.do(ctx, http.MethodGet, []string{"v1", "me"}, nil, true, "", &response)
	return response, err
}

// AdmitJob admits or replays one direct Job using the caller-generated key.
func (c *Client) AdmitJob(ctx context.Context, key string, request controlapi.AdmitJobRequest) (controlapi.Job, error) {
	if strings.TrimSpace(key) == "" {
		return controlapi.Job{}, fmt.Errorf("Idempotency-Key is empty")
	}
	var response controlapi.Job
	err := c.do(ctx, http.MethodPost, []string{"v1", "jobs"}, request, true, key, &response)
	return response, err
}

// Job retrieves one canonical Job snapshot.
func (c *Client) Job(ctx context.Context, id string) (controlapi.Job, error) {
	if id == "" {
		return controlapi.Job{}, fmt.Errorf("Job ID is empty")
	}
	var response controlapi.Job
	err := c.do(ctx, http.MethodGet, []string{"v1", "jobs", id}, nil, true, "", &response)
	return response, err
}

// Cleanup idempotently requests exact cleanup of one Job.
func (c *Client) Cleanup(ctx context.Context, id string) (controlapi.Job, error) {
	if id == "" {
		return controlapi.Job{}, fmt.Errorf("Job ID is empty")
	}
	var response controlapi.Job
	err := c.do(ctx, http.MethodPut, []string{"v1", "jobs", id, "cleanup"}, nil, true, "", &response)
	return response, err
}

func (c *Client) do(ctx context.Context, method string, path []string, input any, authenticated bool, key string, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Dorf API request")
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return fmt.Errorf("create Dorf API request")
	}
	request.Header.Set("Accept", "application/json, application/problem+json")
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}

	response, err := c.http.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Dorf API request failed")
	}
	if response.Body == nil {
		return fmt.Errorf("Dorf API response has no body")
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read Dorf API response")
	}
	if len(contents) > maxResponseBytes {
		return fmt.Errorf("Dorf API response exceeds %d bytes", maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		problem := controlapi.Problem{}
		_ = json.Unmarshal(contents, &problem)
		problem.Status = response.StatusCode
		return &ProblemError{Problem: problem}
	}
	if err := json.Unmarshal(contents, output); err != nil {
		return fmt.Errorf("decode Dorf API response")
	}
	return nil
}

func (c *Client) endpoint(parts []string) string {
	escapedPath := strings.TrimRight(c.base.EscapedPath(), "/")
	for _, part := range parts {
		escapedPath += "/" + url.PathEscape(part)
	}
	path, _ := url.PathUnescape(escapedPath)
	endpoint := *c.base
	endpoint.Path = path
	endpoint.RawPath = escapedPath
	return endpoint.String()
}
