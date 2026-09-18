// Package controlclient is Dorf's small typed remote control HTTP client.
package controlclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/clientconfig"
	"github.com/aphronio/dorf/internal/controlapi"

	"github.com/aphronio/dorf/internal/hostclientconfig"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// A valid 1 MiB goal can occupy more than 6 MiB after JSON escaping.
const maxResponseBytes = 8 << 20

// Client addresses one Deployment and presents one opaque client credential.
type Client struct {
	base       *url.URL
	credential string
	http       *http.Client
	stream     *http.Client
}

// ProblemError retains the stable Problem Details contract without including
// server prose or structured details in its printable error text.
type ProblemError struct {
	Problem controlapi.Problem
}

type serviceError struct{ error }

func (e *serviceError) Unwrap() error { return e.error }

func IsServiceError(err error) bool {
	var service *serviceError
	return errors.As(err, &service)
}

func asServiceError(err error) error {
	if err == nil || IsServiceError(err) {
		return err
	}
	return &serviceError{error: err}
}

func (e *ProblemError) Error() string {
	if e.Problem.Code != "" {
		return fmt.Sprintf("Dorf API request failed with HTTP %d (%s)", e.Problem.Status, e.Problem.Code)
	}
	return fmt.Sprintf("Dorf API request failed with HTTP %d", e.Problem.Status)
}

func (e *ProblemError) Is(target error) bool {
	return e != nil && target == controlapi.ErrFileTooLarge && e.Problem.Status == http.StatusConflict && e.Problem.Code == "file_too_large"
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
	return newClient(base, credential, transport), nil
}

func NewLoopback(credential string) (*Client, error) {
	if credential == "" {
		return nil, fmt.Errorf("Dorf client credential is empty")
	}
	base, err := url.Parse(hostclientconfig.HostOrigin)
	if err != nil {
		return nil, fmt.Errorf("parse Dorf host origin")
	}
	const address = "127.0.0.1:8745"
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, requestedAddress string) (net.Conn, error) {
			if network != "tcp" || requestedAddress != address {
				return nil, fmt.Errorf("Dorf host client refused a non-loopback destination")
			}
			return dialer.DialContext(ctx, "tcp4", address)
		},
	}
	return newClient(base, credential, transport), nil
}

func newClient(base *url.URL, credential string, transport http.RoundTripper) *Client {
	checkRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		base:       base,
		credential: credential,
		http: &http.Client{
			Transport:     transport,
			Timeout:       30 * time.Second,
			CheckRedirect: checkRedirect,
		},
		stream: &http.Client{Transport: transport, CheckRedirect: checkRedirect},
	}
}

// String deliberately excludes the Client credential.
func (c *Client) String() string {
	if c == nil || c.base == nil {
		return "Dorf Client(<nil>)"
	}
	return "Dorf Client(" + c.base.String() + ")"
}

// GoString deliberately excludes the Client credential from %#v formatting.
func (c *Client) GoString() string { return c.String() }

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

// CreateSession admits or replays one direct Session using the caller-generated key.
func (c *Client) CreateSession(ctx context.Context, key string, request controlapi.CreateSessionRequest) (controlapi.Session, error) {
	if strings.TrimSpace(key) == "" {
		return controlapi.Session{}, fmt.Errorf("Idempotency-Key is empty")
	}
	var response controlapi.Session
	err := c.do(ctx, http.MethodPost, []string{"v1", "sessions"}, request, true, key, &response)
	return response, err
}

// Session retrieves one canonical Session snapshot.
func (c *Client) Session(ctx context.Context, id string) (controlapi.Session, error) {
	if id == "" {
		return controlapi.Session{}, fmt.Errorf("Session ID is empty")
	}
	var response controlapi.Session
	err := c.do(ctx, http.MethodGet, []string{"v1", "sessions", id}, nil, true, "", &response)
	return response, err
}

func (c *Client) ListProfiles(ctx context.Context) (controlapi.ProfileList, error) {
	var list controlapi.ProfileList
	err := c.do(ctx, http.MethodGet, []string{"v1", "profiles"}, nil, true, "", &list)
	return list, err
}

// ListSessions retrieves one bounded page of immutable Session references. A zero
// limit asks the server to apply its default.
func (c *Client) ListSessions(ctx context.Context, limit int, cursor string) (controlapi.SessionList, error) {
	if limit < 0 || limit > 100 {
		return controlapi.SessionList{}, fmt.Errorf("Session list limit must be between 1 and 100, or zero for the server default")
	}
	request, err := c.request(ctx, http.MethodGet, []string{"v1", "sessions"}, nil, true, "")
	if err != nil {
		return controlapi.SessionList{}, err
	}
	query := request.URL.Query()
	if limit != 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	request.URL.RawQuery = query.Encode()
	response, err := send(c.http, request)
	if err != nil {
		return controlapi.SessionList{}, asServiceError(err)
	}
	var list controlapi.SessionList
	if err := decodeJSONResponse(response, &list); err != nil {
		return controlapi.SessionList{}, asServiceError(err)
	}
	return list, nil
}

// WatchSession delivers complete canonical snapshots and reconnects an interrupted
// stream using the last successfully delivered event ID. The caller's context
// is the only lifetime limit on the stream.
func (c *Client) WatchSession(ctx context.Context, id string, deliver func(controlapi.Session) error) error {
	if id == "" {
		return fmt.Errorf("Session ID is empty")
	}
	if deliver == nil {
		return fmt.Errorf("Session snapshot receiver is nil")
	}
	lastEventID := ""
	retryAfter := time.Second
	for {
		reconnect, err := c.watchSessionOnce(ctx, id, &lastEventID, &retryAfter, deliver)
		if err != nil && !reconnect {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryAfter):
		}
	}
}

// Retry admits or replays one explicit retry request using caller-retained
// request identity.
func (c *Client) Retry(ctx context.Context, sessionID, key string) (controlapi.Retry, error) {
	if sessionID == "" {
		return controlapi.Retry{}, fmt.Errorf("Session ID is empty")
	}
	if strings.TrimSpace(key) == "" {
		return controlapi.Retry{}, fmt.Errorf("Idempotency-Key is empty")
	}
	var response controlapi.Retry
	err := c.do(ctx, http.MethodPost, []string{"v1", "sessions", sessionID, "retries"}, nil, true, key, &response)
	return response, err
}

// SandboxFile returns the exact file bytes after independently verifying the
// response's SHA-256 Content-Digest. File bodies do not inherit the JSON size
// limit.
func (c *Client) SandboxFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("Sandbox ID is empty")
	}
	if path == "" {
		return nil, fmt.Errorf("Sandbox file path is empty")
	}
	request, err := c.request(ctx, http.MethodGet, []string{"v1", "sandboxes", sandboxID, "files"}, nil, true, "")
	if err != nil {
		return nil, err
	}
	query := request.URL.Query()
	query.Set("path", path)
	request.URL.RawQuery = query.Encode()
	request.Header.Set("Accept", "application/octet-stream, application/problem+json")
	request.Header.Set("Accept-Encoding", "identity")
	response, err := send(c.http, request)
	if err != nil {
		return nil, asServiceError(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, asServiceError(responseProblem(response))
	}
	contents, err := readSandboxFileResponse(response)
	return contents, asServiceError(err)
}

func readSandboxFileResponse(response *http.Response) ([]byte, error) {
	if response.StatusCode != http.StatusOK {
		if response.Body != nil {
			response.Body.Close()
		}
		return nil, fmt.Errorf("Dorf API file response has unexpected HTTP %d", response.StatusCode)
	}
	if response.Body == nil {
		return nil, fmt.Errorf("Dorf API response has no body")
	}
	defer response.Body.Close()
	if response.ContentLength > provider.MaxFileReadBytes {
		return nil, controlapi.ErrFileTooLarge
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, provider.MaxFileReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Dorf API file response")
	}
	if len(contents) > provider.MaxFileReadBytes {
		return nil, controlapi.ErrFileTooLarge
	}
	if response.ContentLength >= 0 && response.ContentLength != int64(len(contents)) {
		return nil, fmt.Errorf("Dorf API file response length does not match Content-Length")
	}
	if err := verifyContentDigest(response.Header.Values("Content-Digest"), contents); err != nil {
		return nil, err
	}
	return contents, nil
}

// Cleanup idempotently requests exact cleanup of one Session.
func (c *Client) Cleanup(ctx context.Context, id string) (controlapi.Session, error) {
	if id == "" {
		return controlapi.Session{}, fmt.Errorf("Session ID is empty")
	}
	var response controlapi.Session
	err := c.do(ctx, http.MethodPut, []string{"v1", "sessions", id, "cleanup"}, nil, true, "", &response)
	return response, err
}

func (c *Client) do(ctx context.Context, method string, path []string, input any, authenticated bool, key string, output any) error {
	request, err := c.request(ctx, method, path, input, authenticated, key)
	if err != nil {
		return err
	}
	response, err := send(c.http, request)
	if err != nil {
		return asServiceError(err)
	}
	return asServiceError(decodeJSONResponse(response, output))
}

func (c *Client) doBody(ctx context.Context, method string, path []string, body []byte, contentType string, authenticated bool, key string, output any) error {
	request, err := c.requestBody(ctx, method, path, bytes.NewReader(body), contentType, authenticated, key)
	if err != nil {
		return err
	}
	response, err := send(c.http, request)
	if err != nil {
		return asServiceError(err)
	}
	return asServiceError(decodeJSONResponse(response, output))
}

func (c *Client) request(ctx context.Context, method string, path []string, input any, authenticated bool, key string) (*http.Request, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, fmt.Errorf("encode Dorf API request")
		}
		body = bytes.NewReader(encoded)
	}
	contentType := ""
	if input != nil {
		contentType = "application/json"
	}
	return c.requestBody(ctx, method, path, body, contentType, authenticated, key)
}

func (c *Client) requestBody(ctx context.Context, method string, path []string, body io.Reader, contentType string, authenticated bool, key string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), body)
	if err != nil {
		return nil, fmt.Errorf("create Dorf API request")
	}
	request.Header.Set("Accept", "application/json, application/problem+json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+c.credential)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	return request, nil
}

func send(client *http.Client, request *http.Request) (*http.Response, error) {
	response, err := client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if request.Context().Err() != nil {
			return nil, request.Context().Err()
		}
		return nil, fmt.Errorf("Dorf API request failed")
	}
	return response, nil
}

func decodeJSONResponse(response *http.Response, output any) error {
	contents, err := readJSONBody(response)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return problemError(response.StatusCode, contents)
	}
	if err := json.Unmarshal(contents, output); err != nil {
		return fmt.Errorf("decode Dorf API response")
	}
	return nil
}

func responseProblem(response *http.Response) error {
	return decodeJSONResponse(response, &struct{}{})
}

func readJSONBody(response *http.Response) ([]byte, error) {
	if response.Body == nil {
		return nil, fmt.Errorf("Dorf API response has no body")
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Dorf API response")
	}
	if len(contents) > maxResponseBytes {
		return nil, fmt.Errorf("Dorf API response exceeds %d bytes", maxResponseBytes)
	}
	return contents, nil
}

func problemError(status int, contents []byte) error {
	problem := controlapi.Problem{}
	_ = json.Unmarshal(contents, &problem)
	problem.Status = status
	return &ProblemError{Problem: problem}
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

func (c *Client) watchSessionOnce(ctx context.Context, id string, lastEventID *string, retryAfter *time.Duration, deliver func(controlapi.Session) error) (bool, error) {
	request, err := c.request(ctx, http.MethodGet, []string{"v1", "sessions", id, "watch"}, nil, true, "")
	if err != nil {
		return false, err
	}
	request.Header.Set("Accept", "text/event-stream")
	if *lastEventID != "" {
		request.Header.Set("Last-Event-ID", *lastEventID)
	}
	response, err := send(c.stream, request)
	if err != nil {
		return ctx.Err() == nil, asServiceError(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := response.StatusCode
		err := responseProblem(response)
		var problem *ProblemError
		retryable := status >= 500
		if errors.As(err, &problem) {
			retryable = retryable || problem.Problem.Retryable
		}
		return retryable, asServiceError(err)
	}
	if response.Body == nil {
		return false, asServiceError(fmt.Errorf("Dorf API response has no body"))
	}
	defer response.Body.Close()
	if mediaType, _, _ := strings.Cut(response.Header.Get("Content-Type"), ";"); strings.TrimSpace(mediaType) != "text/event-stream" {
		return false, asServiceError(fmt.Errorf("Dorf API watch response is not an event stream"))
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), maxResponseBytes+1024)
	eventType := ""
	eventID := ""
	data := make([]string, 0, 1)
	dataBytes := 0
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) != 0 && (eventType == "" || eventType == "snapshot") {
				session, err := decodeSession([]byte(strings.Join(data, "\n")))
				if err != nil {
					return false, asServiceError(fmt.Errorf("decode Dorf API watch snapshot"))
				}
				if err := deliver(session); err != nil {
					return false, err
				}
			}
			if eventID != "" {
				*lastEventID = eventID
			}
			eventType, eventID, data, dataBytes = "", "", data[:0], 0
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		} else {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventType = value
		case "id":
			if !strings.ContainsRune(value, 0) {
				eventID = value
			}
		case "data":
			dataBytes += len(value)
			if len(data) != 0 {
				dataBytes++
			}
			if dataBytes > maxResponseBytes {
				return false, asServiceError(fmt.Errorf("Dorf API watch snapshot exceeds %d bytes", maxResponseBytes))
			}
			data = append(data, value)
		case "retry":
			milliseconds, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr == nil && milliseconds >= 0 && milliseconds <= int64((24*time.Hour)/time.Millisecond) {
				*retryAfter = time.Duration(milliseconds) * time.Millisecond
			}
		}
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if scanner.Err() != nil {
		return true, asServiceError(fmt.Errorf("read Dorf API watch stream"))
	}
	return true, nil
}

func decodeSession(contents []byte) (controlapi.Session, error) {
	var session controlapi.Session
	if err := json.Unmarshal(contents, &session); err != nil {
		return controlapi.Session{}, fmt.Errorf("decode Dorf API Session response")
	}
	return session, nil
}

func verifyContentDigest(values []string, contents []byte) error {
	if len(values) != 1 {
		return fmt.Errorf("Dorf API file response requires one Content-Digest")
	}
	found := false
	var expected []byte
	for _, member := range strings.Split(values[0], ",") {
		algorithm, encoded, ok := strings.Cut(strings.TrimSpace(member), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(algorithm), "sha-256") {
			continue
		}
		if found {
			return fmt.Errorf("Dorf API file response has duplicate SHA-256 Content-Digest values")
		}
		found = true
		encoded = strings.TrimSpace(encoded)
		if len(encoded) < 2 || encoded[0] != ':' || encoded[len(encoded)-1] != ':' {
			return fmt.Errorf("Dorf API file response has invalid Content-Digest")
		}
		var err error
		expected, err = base64.StdEncoding.Strict().DecodeString(encoded[1 : len(encoded)-1])
		if err != nil || len(expected) != sha256.Size {
			return fmt.Errorf("Dorf API file response has invalid Content-Digest")
		}
	}
	if !found {
		return fmt.Errorf("Dorf API file response has no SHA-256 Content-Digest")
	}
	actual := sha256.Sum256(contents)
	if !bytes.Equal(expected, actual[:]) {
		return fmt.Errorf("Dorf API file response does not match Content-Digest")
	}
	return nil
}
