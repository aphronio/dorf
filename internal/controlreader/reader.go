// Package controlreader exposes the fixed read-only external observations
// needed by the control API without granting that API provider credentials or
// provider mutation capabilities.
package controlreader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

const (
	MaxRequestBytes     = 8 << 10
	MaxObservationBytes = 16 << 20
	maxProblemBytes     = 4 << 10
	clientTimeout       = 20 * time.Second
	handlerTimeout      = 18 * time.Second

	HealthPath             = "/v1/health"
	FileReadPath           = "/v1/files/read"
	MessageObservationPath = "/v1/messages/observe"
	DefaultConnectionPath  = "/v1/admission/default-connection"
	ConnectionCheckPath    = "/v1/admission/check-connection"
	GitHubInstallationPath = "/v1/admission/github-installation"
	PullRequestPath        = "/v1/github/pull-request/observe"
)

var (
	ErrUnauthorized     = errors.New("control reader authentication failed")
	ErrInvalidRequest   = errors.New("control reader request is invalid")
	ErrSandboxNotFound  = errors.New("control reader Sandbox not found")
	ErrInvalidFilePath  = errors.New("control reader file path is invalid")
	ErrFileNotFound     = errors.New("control reader file is unavailable")
	ErrUnavailable      = errors.New("control reader observation is unavailable")
	ErrResponseTooLarge = errors.New("control reader response exceeds its bound")
)

// Store is the durable custody needed to prove one read belongs to one Job.
// The provider-facing process receives no alternate resource or profile input.
type Store interface {
	Job(context.Context, string) (core.Job, error)
	CodingJob(context.Context, string) (coding.Job, error)
	Proposal(context.Context, string) (*coding.Proposal, error)
	Sandbox(context.Context, string) (core.Sandbox, error)
	AgentMessageExecution(context.Context, string) (core.AgentMessageExecution, error)
	WithJobFence(context.Context, string, func() error) error
}

type AdmissionProvider interface {
	DefaultConnection() (string, error)
	Check(context.Context, string) error
}

type InstallationDiscovery interface {
	DiscoverInstallation(context.Context, string) (string, error)
}

type PullRequestObservation interface {
	PullRequest(context.Context, githubapi.Authority, int64) (githubapi.PullRequest, error)
}

// Service owns provider-facing reads. It accepts only durable Dorf identities
// and one already-validated workspace-relative path.
type Service struct {
	Store         Store
	Runtimes      core.SandboxRuntimeResolver
	Provider      AdmissionProvider
	Installations InstallationDiscovery
	PullRequests  PullRequestObservation
}

func (s Service) ReadFile(ctx context.Context, sandboxID, relativePath string) ([]byte, error) {
	if !validIdentity(sandboxID) {
		return nil, ErrSandboxNotFound
	}
	if err := provider.ValidateWorkspaceRelativePath(relativePath); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidFilePath, err)
	}
	if s.Store == nil || s.Runtimes == nil {
		return nil, fmt.Errorf("control reader file authority is not configured")
	}
	owned, err := s.Store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return nil, ErrSandboxNotFound
	}
	if err != nil {
		return nil, err
	}
	if owned.ID != sandboxID || !validIdentity(owned.JobID) || !validIdentity(owned.OwnershipNonce) {
		return nil, ErrUnavailable
	}

	var contents []byte
	err = s.Store.WithJobFence(ctx, owned.JobID, func() error {
		job, err := s.Store.Job(ctx, owned.JobID)
		if errors.Is(err, postgres.ErrNotFound) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		if job.ID != owned.JobID || job.CleanupState != core.CleanupPending {
			return ErrUnavailable
		}
		current, err := s.Store.Sandbox(ctx, sandboxID)
		if errors.Is(err, postgres.ErrNotFound) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		if current.ID != sandboxID || current.JobID != job.ID || current.OwnershipNonce != owned.OwnershipNonce {
			return ErrUnavailable
		}
		runtime, err := s.Runtimes.ResolveSandbox(ctx, job.SandboxProfile)
		if err != nil {
			return fmt.Errorf("resolve Sandbox profile for file read: %w", err)
		}
		if runtime.SandboxProfile != job.SandboxProfile || runtime.Files == nil {
			return fmt.Errorf("resolved Sandbox runtime has no exact file authority")
		}
		contents, err = runtime.Files.ReadSandboxFile(ctx, job, current, relativePath)
		switch {
		case errors.Is(err, provider.ErrInvalidFilePath):
			return ErrInvalidFilePath
		case errors.Is(err, provider.ErrFileUnavailable):
			return ErrFileNotFound
		default:
			return err
		}
	})
	if err != nil {
		return nil, err
	}
	return contents, nil
}

func (s Service) ObserveMessage(ctx context.Context, jobID, messageID string) (core.MessageResult, error) {
	if !validIdentity(jobID) || !validIdentity(messageID) {
		return core.MessageResult{}, ErrUnavailable
	}
	if s.Store == nil || s.Runtimes == nil {
		return core.MessageResult{}, fmt.Errorf("control reader Message authority is not configured")
	}
	var result core.MessageResult
	err := s.Store.WithJobFence(ctx, jobID, func() error {
		authoritative, err := s.Store.AgentMessageExecution(ctx, messageID)
		if errors.Is(err, postgres.ErrNotFound) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		job := authoritative.Job
		if job.ID != jobID || job.CleanupState != core.CleanupPending ||
			authoritative.Message.ID != messageID || authoritative.Message.JobID != job.ID ||
			authoritative.AgentRun.JobID != job.ID || authoritative.AgentRun.MessageID != messageID ||
			!validIdentity(authoritative.AgentRun.ID) || authoritative.AgentRun.State != core.AgentRunCompleted ||
			!terminalMessageOutcome(authoritative.AgentRun.TurnOutcome) ||
			!validIdentity(authoritative.Sandbox.ID) || authoritative.Sandbox.JobID != job.ID ||
			!validIdentity(authoritative.Sandbox.OwnershipNonce) || authoritative.AgentRun.SandboxID != authoritative.Sandbox.ID {
			return ErrUnavailable
		}
		runtime, err := s.Runtimes.ResolveSandbox(ctx, job.SandboxProfile)
		if err != nil {
			return fmt.Errorf("resolve Sandbox profile for Message observation: %w", err)
		}
		if runtime.SandboxProfile != job.SandboxProfile || runtime.Execution == nil {
			return fmt.Errorf("resolved Sandbox runtime has no exact Message observation authority")
		}
		result, err = runtime.Execution.ObserveSettledAgentMessage(ctx, job.ID, messageID)
		if err != nil {
			return err
		}
		if result.MessageID != messageID {
			return ErrUnavailable
		}
		if !result.Terminal() || result.Outcome != authoritative.AgentRun.TurnOutcome {
			return ErrUnavailable
		}
		if len(result.Output) > MaxObservationBytes {
			return ErrResponseTooLarge
		}
		return nil
	})
	if err != nil {
		return core.MessageResult{}, err
	}
	return result, nil
}

func terminalMessageOutcome(outcome string) bool {
	return outcome == "completed" || outcome == "failed" || outcome == "interrupted"
}

func (s Service) DefaultConnection() (string, error) {
	if s.Provider == nil {
		return "", fmt.Errorf("AI connection observation authority is not configured")
	}
	connection, err := s.Provider.DefaultConnection()
	if err != nil {
		return "", err
	}
	if !validIdentity(connection) {
		return "", fmt.Errorf("default AI connection returned invalid identity")
	}
	return connection, nil
}

func (s Service) Check(ctx context.Context, connection string) error {
	if !validIdentity(connection) {
		return ErrInvalidRequest
	}
	if s.Provider == nil {
		return fmt.Errorf("AI connection observation authority is not configured")
	}
	return s.Provider.Check(ctx, connection)
}

func (s Service) DiscoverInstallation(ctx context.Context, repository string) (string, error) {
	if !validRepository(repository) {
		return "", ErrInvalidRequest
	}
	if s.Installations == nil {
		return "", fmt.Errorf("GitHub installation observation authority is not configured")
	}
	installation, err := s.Installations.DiscoverInstallation(ctx, repository)
	if err != nil {
		return "", err
	}
	if !validIdentity(installation) {
		return "", fmt.Errorf("GitHub installation observation returned invalid identity")
	}
	return installation, nil
}

func (s Service) ObservePullRequest(ctx context.Context, jobID string) (githubapi.PullRequest, error) {
	if !validIdentity(jobID) {
		return githubapi.PullRequest{}, ErrUnavailable
	}
	if s.Store == nil || s.PullRequests == nil {
		return githubapi.PullRequest{}, fmt.Errorf("GitHub pull-request observation authority is not configured")
	}
	job, err := s.Store.CodingJob(ctx, jobID)
	if errors.Is(err, postgres.ErrNotFound) {
		return githubapi.PullRequest{}, ErrUnavailable
	}
	if err != nil {
		return githubapi.PullRequest{}, err
	}
	proposal, err := s.Store.Proposal(ctx, jobID)
	if errors.Is(err, postgres.ErrNotFound) {
		return githubapi.PullRequest{}, ErrUnavailable
	}
	if err != nil {
		return githubapi.PullRequest{}, err
	}
	if job.ID != jobID || !validRepository(job.GitHubRepository) || !validIdentity(job.GitHubInstallation) ||
		!validIdentity(job.Branch) || !validIdentity(job.BaseBranch) || !validIdentity(job.Revision) ||
		proposal == nil || proposal.JobID != job.ID || proposal.Number < 1 || !validIdentity(proposal.URL) ||
		proposal.ProposedRevision != job.Revision {
		return githubapi.PullRequest{}, ErrUnavailable
	}
	authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
	pull, err := s.PullRequests.PullRequest(ctx, authority, proposal.Number)
	if err != nil {
		return githubapi.PullRequest{}, fmt.Errorf("observe exact GitHub pull request #%d: %w", proposal.Number, err)
	}
	if pull.Number != proposal.Number || pull.URL != proposal.URL || pull.Repository != job.GitHubRepository ||
		pull.Head != job.Branch || pull.Base != job.BaseBranch || pull.HeadSHA != proposal.ProposedRevision {
		return githubapi.PullRequest{}, ErrUnavailable
	}
	return pull, nil
}

func validIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 255 && !strings.ContainsRune(value, 0)
}

func validRepository(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 255 || strings.ContainsAny(value, "\x00\\?#") {
		return false
	}
	owner, name, found := strings.Cut(value, "/")
	return found && owner != "" && name != "" && !strings.Contains(name, "/") && owner != "." && owner != ".." && name != "." && name != ".."
}

type fileReadRequest struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

type messageObservationRequest struct {
	JobID     string `json:"job_id"`
	MessageID string `json:"message_id"`
}

type connectionRequest struct {
	Connection string `json:"connection"`
}

type installationRequest struct {
	Repository string `json:"repository"`
}

type pullRequestObservationRequest struct {
	JobID string `json:"job_id"`
}

type connectionResponse struct {
	Connection string `json:"connection"`
}

type installationResponse struct {
	Installation string `json:"installation"`
}

type problem struct {
	Code string `json:"code"`
}

type healthResponse struct {
	Ready bool `json:"ready"`
}

// NewHandler returns the complete fixed internal HTTP surface. Authentication
// is checked before parsing or consulting durable/provider authority.
func NewHandler(token string, service Service) (http.Handler, error) {
	if !validToken(token) {
		return nil, fmt.Errorf("control reader token must be one 256-bit lowercase hex value")
	}
	routes := map[string]http.HandlerFunc{
		HealthPath: jsonEndpoint(0, func(context.Context, struct{}) (healthResponse, error) {
			return healthResponse{Ready: true}, nil
		}),
		FileReadPath: fileReadEndpoint(service),
		MessageObservationPath: jsonEndpoint(MaxObservationBytes, func(ctx context.Context, input messageObservationRequest) (core.MessageResult, error) {
			return service.ObserveMessage(ctx, input.JobID, input.MessageID)
		}),
		DefaultConnectionPath: jsonEndpoint(0, func(context.Context, struct{}) (connectionResponse, error) {
			connection, err := service.DefaultConnection()
			return connectionResponse{Connection: connection}, err
		}),
		ConnectionCheckPath: jsonEndpoint(0, func(ctx context.Context, input connectionRequest) (struct{}, error) {
			return struct{}{}, service.Check(ctx, input.Connection)
		}),
		GitHubInstallationPath: jsonEndpoint(0, func(ctx context.Context, input installationRequest) (installationResponse, error) {
			installation, err := service.DiscoverInstallation(ctx, input.Repository)
			return installationResponse{Installation: installation}, err
		}),
		PullRequestPath: jsonEndpoint(MaxObservationBytes, func(ctx context.Context, input pullRequestObservationRequest) (githubapi.PullRequest, error) {
			return service.ObservePullRequest(ctx, input.JobID)
		}),
	}

	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !authenticated(r, expected) {
			writeProblem(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), handlerTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeProblem(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		if r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
			writeProblem(w, http.StatusBadRequest, "invalid_request")
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeProblem(w, http.StatusUnsupportedMediaType, "invalid_content_type")
			return
		}
		handle, found := routes[r.URL.Path]
		if !found {
			writeProblem(w, http.StatusNotFound, "not_found")
			return
		}
		handle(w, r)
	}), nil
}

func jsonEndpoint[Input, Output any](maxResponseBytes int, call func(context.Context, Input) (Output, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input Input
		if !decodeRequest(w, r, &input) {
			return
		}
		output, err := call(r.Context(), input)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		contents, err := marshalJSON(output)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if maxResponseBytes > 0 && len(contents) > maxResponseBytes {
			writeProblem(w, http.StatusConflict, "response_too_large")
			return
		}
		writeJSONBytes(w, http.StatusOK, contents)
	}
}

func fileReadEndpoint(service Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input fileReadRequest
		if !decodeRequest(w, r, &input) {
			return
		}
		contents, err := service.ReadFile(r.Context(), input.SandboxID, input.Path)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(contents)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(contents)
	}
}

func authenticated(r *http.Request, expected [sha256.Size]byte) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) != len("Bearer ")+64 || !strings.HasPrefix(values[0], "Bearer ") {
		return false
	}
	candidate := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
	return subtle.ConstantTimeCompare(candidate[:], expected[:]) == 1
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.ContentLength > MaxRequestBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return false
	}
	contents, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if len(contents) > MaxRequestBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeProblem(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidRequest):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_request")
	case errors.Is(err, ErrSandboxNotFound):
		writeProblem(w, http.StatusNotFound, "sandbox_not_found")
	case errors.Is(err, ErrInvalidFilePath):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_file_path")
	case errors.Is(err, ErrFileNotFound):
		writeProblem(w, http.StatusNotFound, "file_not_found")
	case errors.Is(err, ErrUnavailable):
		writeProblem(w, http.StatusConflict, "unavailable")
	case errors.Is(err, ErrResponseTooLarge):
		writeProblem(w, http.StatusConflict, "response_too_large")
	default:
		writeProblem(w, http.StatusInternalServerError, "internal_error")
	}
}

func writeProblem(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, problem{Code: code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	contents, err := marshalJSON(value)
	if err != nil {
		status, contents = http.StatusInternalServerError, []byte("{\"code\":\"internal_error\"}\n")
	}
	writeJSONBytes(w, status, contents)
}

func marshalJSON(value any) ([]byte, error) {
	contents, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(contents, '\n'), nil
}

func writeJSONBytes(w http.ResponseWriter, status int, contents []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(contents)))
	w.WriteHeader(status)
	_, _ = w.Write(contents)
}

// Client is the control API's only provider-facing observation capability.
type Client struct {
	origin string
	token  string
	http   *http.Client
}

func NewClient(origin, token string, client *http.Client) (Client, error) {
	origin = strings.TrimSpace(origin)
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" || parsed.Port() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return Client{}, fmt.Errorf("control reader origin must be one exact internal HTTP origin")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1024 || port > 65535 {
		return Client{}, fmt.Errorf("control reader origin must use port 1024-65535")
	}
	if !validToken(token) {
		return Client{}, fmt.Errorf("control reader token must be one 256-bit lowercase hex value")
	}
	if client == nil {
		client = &http.Client{}
	}
	configured := *client
	if configured.Timeout <= 0 || configured.Timeout > clientTimeout {
		configured.Timeout = clientTimeout
	}
	if configured.Transport == nil {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if ok {
			transport = transport.Clone()
		} else {
			transport = &http.Transport{}
		}
		transport.Proxy = nil
		configured.Transport = transport
	} else if transport, ok := configured.Transport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.Proxy = nil
		configured.Transport = transport
	}
	configured.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return Client{origin: origin, token: token, http: &configured}, nil
}

func (c Client) Health(ctx context.Context) error {
	response, err := c.request(ctx, HealthPath, struct{}{})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeProblem(response)
	}
	var result healthResponse
	if err := decodeJSONResponse(response, &result, maxProblemBytes, "JSON"); err != nil {
		return err
	}
	if !result.Ready {
		return ErrUnavailable
	}
	return nil
}

func (c Client) ReadFile(ctx context.Context, sandboxID, relativePath string) ([]byte, error) {
	response, err := c.request(ctx, FileReadPath, fileReadRequest{SandboxID: sandboxID, Path: relativePath})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, decodeProblem(response)
	}
	if response.Header.Get("Content-Type") != "application/octet-stream" {
		return nil, fmt.Errorf("control reader returned an invalid file content type")
	}
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read control reader response: %w", err)
	}
	if response.ContentLength >= 0 && response.ContentLength != int64(len(contents)) {
		return nil, fmt.Errorf("control reader returned a conflicting file length")
	}
	return contents, nil
}

func (c Client) ObserveMessage(ctx context.Context, jobID, messageID string) (core.MessageResult, error) {
	response, err := c.request(ctx, MessageObservationPath, messageObservationRequest{JobID: jobID, MessageID: messageID})
	if err != nil {
		return core.MessageResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return core.MessageResult{}, decodeProblem(response)
	}
	var result core.MessageResult
	if err := decodeJSONResponse(response, &result, MaxObservationBytes, "Message observation"); err != nil {
		return core.MessageResult{}, err
	}
	if result.MessageID != messageID || !result.Terminal() || len(result.Output) > MaxObservationBytes {
		return core.MessageResult{}, ErrUnavailable
	}
	return result, nil
}

func (c Client) DefaultConnection() (string, error) {
	response, err := c.request(context.Background(), DefaultConnectionPath, struct{}{})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", decodeProblem(response)
	}
	var result connectionResponse
	if err := decodeJSONResponse(response, &result, maxProblemBytes, "JSON"); err != nil {
		return "", err
	}
	if !validIdentity(result.Connection) {
		return "", ErrUnavailable
	}
	return result.Connection, nil
}

func (c Client) Check(ctx context.Context, connection string) error {
	response, err := c.request(ctx, ConnectionCheckPath, connectionRequest{Connection: connection})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return decodeProblem(response)
	}
	var result struct{}
	return decodeJSONResponse(response, &result, maxProblemBytes, "JSON")
}

func (c Client) DiscoverInstallation(ctx context.Context, repository string) (string, error) {
	response, err := c.request(ctx, GitHubInstallationPath, installationRequest{Repository: repository})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", decodeProblem(response)
	}
	var result installationResponse
	if err := decodeJSONResponse(response, &result, maxProblemBytes, "JSON"); err != nil {
		return "", err
	}
	if !validIdentity(result.Installation) {
		return "", ErrUnavailable
	}
	return result.Installation, nil
}

func (c Client) ObservePullRequest(ctx context.Context, jobID string) (githubapi.PullRequest, error) {
	response, err := c.request(ctx, PullRequestPath, pullRequestObservationRequest{JobID: jobID})
	if err != nil {
		return githubapi.PullRequest{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return githubapi.PullRequest{}, decodeProblem(response)
	}
	var pull githubapi.PullRequest
	if err := decodeJSONResponse(response, &pull, MaxObservationBytes, "pull-request observation"); err != nil {
		return githubapi.PullRequest{}, err
	}
	if pull.Number < 1 || !validRepository(pull.Repository) || !validIdentity(pull.URL) ||
		!validIdentity(pull.Head) || !validIdentity(pull.Base) || !validIdentity(pull.HeadSHA) {
		return githubapi.PullRequest{}, ErrUnavailable
	}
	return pull, nil
}

func decodeJSONResponse(response *http.Response, target any, maxBytes int, name string) error {
	if !jsonContentType(response.Header.Get("Content-Type")) {
		return fmt.Errorf("control reader returned an invalid %s content type", name)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return fmt.Errorf("read %s response: %w", name, err)
	}
	if len(contents) > maxBytes {
		return ErrResponseTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s response: %w", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s response: trailing JSON", name)
	}
	return nil
}

func jsonContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func (c Client) request(ctx context.Context, path string, input any) (*http.Response, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call control reader: %w", err)
	}
	return response, nil
}

func decodeProblem(response *http.Response) error {
	var value problem
	if err := decodeJSONResponse(response, &value, maxProblemBytes, "JSON"); err != nil || !problemMatchesStatus(value.Code, response.StatusCode) {
		return fmt.Errorf("control reader returned HTTP %d", response.StatusCode)
	}
	switch value.Code {
	case "unauthorized":
		return ErrUnauthorized
	case "invalid_request":
		return ErrInvalidRequest
	case "sandbox_not_found":
		return ErrSandboxNotFound
	case "invalid_file_path":
		return ErrInvalidFilePath
	case "file_not_found":
		return ErrFileNotFound
	case "unavailable":
		return ErrUnavailable
	case "response_too_large":
		return ErrResponseTooLarge
	default:
		return fmt.Errorf("control reader returned HTTP %d", response.StatusCode)
	}
}

func problemMatchesStatus(code string, status int) bool {
	switch code {
	case "unauthorized":
		return status == http.StatusUnauthorized
	case "invalid_request":
		return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
	case "sandbox_not_found", "file_not_found":
		return status == http.StatusNotFound
	case "invalid_file_path":
		return status == http.StatusUnprocessableEntity
	case "unavailable", "response_too_large":
		return status == http.StatusConflict
	default:
		return false
	}
}

func validToken(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}
