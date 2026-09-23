// Package controlreader exposes fixed external observations and bounded workspace
// file writes without granting the control API provider credentials.
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

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

const (
	MaxRequestBytes     = 8 << 10
	MaxObservationBytes = 16 << 20
	maxProblemBytes     = 4 << 10
	clientTimeout       = 20 * time.Second
	handlerTimeout      = 18 * time.Second

	HealthPath            = "/v1/health"
	FileWritePath         = "/v1/files/write"
	FileReadPath          = "/v1/files/read"
	DefaultConnectionPath = "/v1/admission/default-connection"
	DefaultModelPath      = "/v1/admission/default-model"
	ConnectionCheckPath   = "/v1/admission/check-connection"
)

var (
	ErrUnauthorized     = errors.New("control reader authentication failed")
	ErrInvalidRequest   = errors.New("control reader request is invalid")
	ErrSandboxNotFound  = errors.New("control reader Sandbox not found")
	ErrInvalidFilePath  = errors.New("control reader file path is invalid")
	ErrFileNotFound     = errors.New("control reader file is unavailable")
	ErrFileTooLarge     = errors.New("control reader file exceeds read limit")
	ErrUnavailable      = errors.New("control reader observation is unavailable")
	ErrResponseTooLarge = errors.New("control reader response exceeds its bound")
)

// Store is the durable custody needed to prove one read belongs to one Session.
// The provider-facing process receives no alternate resource or profile input.
type Store interface {
	SandboxDeliveryHeld(context.Context, string) (bool, error)
	core.SandboxActivityStore
	Session(context.Context, string) (core.Session, error)
	Sandbox(context.Context, string) (core.Sandbox, error)
	WithSessionFence(context.Context, string, func() error) error
}

type AdmissionProvider interface {
	DefaultConnection() (string, error)
	DefaultModel(string) (string, error)
	Check(context.Context, string) error
}

// Service owns provider-facing reads. It accepts only durable Dorf identities
// and one already-validated Sandbox file path.
type Service struct {
	Workspace            func(context.Context, core.Session) (persistence.Workspace, error)
	ObservationAttention func(context.Context, core.Session) (string, error)
	Replies              *codex.ReplyFeed
	Store                Store
	Runtimes             core.SandboxRuntimeResolver
	Provider             AdmissionProvider
}

func (s Service) ReadFile(ctx context.Context, sandboxID, relativePath string) ([]byte, error) {
	if err := provider.ValidateFilePath(relativePath); err != nil {
		return nil, ErrInvalidFilePath
	}
	var contents []byte
	err := s.withFileSandbox(ctx, sandboxID, func(runtime core.SandboxRuntime, session core.Session, owned core.Sandbox) error {
		if runtime.Files == nil {
			return ErrUnavailable
		}
		var err error
		contents, err = runtime.Files.ReadSandboxFile(ctx, session, owned, relativePath)
		if err != nil {
			contents = nil
		}
		if err == nil && len(contents) > provider.MaxFileReadBytes {
			contents = nil
			return ErrFileTooLarge
		}
		return err
	})
	return contents, err
}

func (s Service) withSandbox(ctx context.Context, sandboxID string, call func(core.SandboxRuntime, core.Session, core.Sandbox) error) error {
	return s.accessSandbox(ctx, sandboxID, true, call)
}

func (s Service) withFileSandbox(ctx context.Context, sandboxID string, call func(core.SandboxRuntime, core.Session, core.Sandbox) error) error {
	return s.accessSandboxMode(ctx, sandboxID, true, true, call)
}

func (s Service) accessSandbox(ctx context.Context, sandboxID string, reconcileIdle bool, call func(core.SandboxRuntime, core.Session, core.Sandbox) error) error {
	return s.accessSandboxMode(ctx, sandboxID, reconcileIdle, false, call)
}

func (s Service) accessSandboxMode(ctx context.Context, sandboxID string, reconcileIdle, allowBranchFiles bool, call func(core.SandboxRuntime, core.Session, core.Sandbox) error) error {
	if !validIdentity(sandboxID) {
		return ErrSandboxNotFound
	}
	if s.Store == nil || s.Runtimes == nil {
		return fmt.Errorf("control reader file authority is not configured")
	}
	owned, err := s.Store.Sandbox(ctx, sandboxID)
	if errors.Is(err, postgres.ErrNotFound) {
		return ErrSandboxNotFound
	}
	if err != nil {
		return err
	}
	if owned.ID != sandboxID || !validIdentity(owned.SessionID) || !validIdentity(owned.OwnershipNonce) {
		return ErrUnavailable
	}

	var idleRuntime core.Execution
	defer func() {
		if reconcileIdle {
			core.ReconcileIdle(ctx, idleRuntime, owned.SessionID)
		}
	}()
	err = s.Store.WithSessionFence(ctx, owned.SessionID, func() error {
		runtime, session, err := s.sandboxAuthority(ctx, owned, allowBranchFiles)
		if err != nil {
			return err
		}
		idleRuntime = runtime.Execution
		if reconcileIdle {
			err = core.WithSandboxActivity(ctx, s.Store, session.ID, func() error { return call(runtime, session, owned) })
		} else {
			err = call(runtime, session, owned)
		}
		switch {
		case errors.Is(err, provider.ErrInvalidFilePath):
			return ErrInvalidFilePath
		case errors.Is(err, provider.ErrFileUnavailable):
			return ErrFileNotFound
		case errors.Is(err, provider.ErrFileTooLarge):
			return ErrFileTooLarge
		default:
			return err
		}
	})
	return err
}

func (s Service) sandboxAuthority(ctx context.Context, owned core.Sandbox, allowBranchFiles bool) (core.SandboxRuntime, core.Session, error) {
	session, err := s.Store.Session(ctx, owned.SessionID)
	if errors.Is(err, postgres.ErrNotFound) {
		return core.SandboxRuntime{}, core.Session{}, ErrUnavailable
	}
	if err != nil {
		return core.SandboxRuntime{}, core.Session{}, err
	}
	if session.ID != owned.SessionID || session.CleanupState != core.CleanupPending {
		return core.SandboxRuntime{}, core.Session{}, ErrUnavailable
	}
	current, err := s.Store.Sandbox(ctx, owned.ID)
	if errors.Is(err, postgres.ErrNotFound) {
		return core.SandboxRuntime{}, core.Session{}, ErrUnavailable
	}
	if err != nil {
		return core.SandboxRuntime{}, core.Session{}, err
	}
	if current != owned {
		return core.SandboxRuntime{}, core.Session{}, ErrUnavailable
	}
	held, err := s.Store.SandboxDeliveryHeld(ctx, owned.ID)
	if err != nil {
		return core.SandboxRuntime{}, core.Session{}, err
	}
	if held {
		if err := s.authorizeHeldFilePreparation(ctx, owned.ID, allowBranchFiles); err != nil {
			return core.SandboxRuntime{}, core.Session{}, err
		}
	}
	runtime, err := s.Runtimes.ResolveSandbox(ctx, session.ProfileRef())
	if err != nil {
		return core.SandboxRuntime{}, core.Session{}, fmt.Errorf("resolve Sandbox profile for file read: %w", err)
	}
	if runtime.SandboxProfile != session.ProfileRef() {
		return core.SandboxRuntime{}, core.Session{}, fmt.Errorf("resolved Sandbox runtime has a different profile")
	}
	return runtime, session, nil
}

func (s Service) authorizeHeldFilePreparation(ctx context.Context, sandboxID string, allow bool) error {
	if !allow {
		return ErrUnavailable
	}
	preparer, ok := s.Store.(interface {
		CheckpointBranchFilePreparationAllowed(context.Context, string) (bool, error)
	})
	if !ok {
		return ErrUnavailable
	}
	allowed, err := preparer.CheckpointBranchFilePreparationAllowed(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrUnavailable
	}
	return nil
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

func (s Service) DefaultModel(connection string) (string, error) {
	if !validIdentity(connection) {
		return "", ErrInvalidRequest
	}
	if s.Provider == nil {
		return "", fmt.Errorf("AI connection observation authority is not configured")
	}
	model, err := s.Provider.DefaultModel(connection)
	if err != nil {
		return "", err
	}
	if !validModel(model) {
		return "", fmt.Errorf("AI connection returned invalid default model")
	}
	return model, nil
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

func validIdentity(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 255 && !strings.ContainsRune(value, 0)
}

func validModel(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= 1024 && !strings.ContainsRune(value, 0)
}

type fileReadRequest struct {
	SandboxID string `json:"sandbox_id"`
	Path      string `json:"path"`
}

type connectionRequest struct {
	Connection string `json:"connection"`
}

type connectionResponse struct {
	Connection string `json:"connection"`
}

type modelResponse struct {
	Model string `json:"model"`
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
	fileTransfers := make(chan struct{}, provider.MaxConcurrentFileReads)
	routes := map[string]http.HandlerFunc{
		InputCapabilitiesPath: jsonEndpoint(MaxRequestBytes, func(ctx context.Context, input observationRequest) (core.InputCapabilities, error) {
			return service.InputCapabilities(ctx, input.SessionID)
		}),
		WorkspacePath: jsonEndpoint(MaxRequestBytes, func(ctx context.Context, input observationRequest) (persistence.Workspace, error) {
			return service.ReadWorkspace(ctx, input.SessionID)
		}),
		NativeEventPath: jsonEndpoint(MaxRequestBytes, func(ctx context.Context, input nativeEventRequest) (core.NativeAcknowledgement, error) {
			return service.SubmitEvent(ctx, input.SessionID, input.Event)
		}),
		NativeTurnsPath: jsonEndpoint(MaxObservationBytes, func(ctx context.Context, input observationRequest) (core.HarnessHistory, error) {
			return service.ReadNativeTurns(ctx, input.SessionID)
		}),
		CoherentObservationPath: jsonEndpoint(MaxObservationBytes, func(ctx context.Context, input observationRequest) (TurnObservation, error) {
			return service.ReadTurnObservation(ctx, input.SessionID, input.TurnID, input.Cursor)
		}),
		ObservationStreamPath: observationStreamEndpoint(service),
		HealthPath: jsonEndpoint(0, func(context.Context, struct{}) (healthResponse, error) {
			return healthResponse{Ready: true}, nil
		}),
		FileReadPath:  fileReadEndpoint(service, fileTransfers),
		FileWritePath: fileWriteEndpoint(service),
		CommandPath:   commandEndpoint(service),
		StatusPath:    statusEndpoint(service),
		TimelinePath: jsonEndpoint(MaxObservationBytes, func(ctx context.Context, input timelineRequest) (core.HarnessTimeline, error) {
			return service.ReadTimeline(ctx, input.SessionID, input.TurnID)
		}),
		DefaultConnectionPath: jsonEndpoint(0, func(context.Context, struct{}) (connectionResponse, error) {
			connection, err := service.DefaultConnection()
			return connectionResponse{Connection: connection}, err
		}),
		DefaultModelPath: jsonEndpoint(0, func(_ context.Context, input connectionRequest) (modelResponse, error) {
			model, err := service.DefaultModel(input.Connection)
			return modelResponse{Model: model}, err
		}),
		ConnectionCheckPath: jsonEndpoint(0, func(ctx context.Context, input connectionRequest) (struct{}, error) {
			return struct{}{}, service.Check(ctx, input.Connection)
		}),
	}

	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !authenticated(r, expected) {
			writeProblem(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		timeout := handlerTimeout
		if r.URL.Path == ObservationStreamPath {
			timeout = ObservationStreamTimeout
		}
		if r.URL.Path == CommandPath {
			timeout = provider.CommandTransportTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
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

func fileReadEndpoint(service Service, transfers chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input fileReadRequest
		if !decodeRequest(w, r, &input) {
			return
		}
		select {
		case transfers <- struct{}{}:
			defer func() { <-transfers }()
		case <-r.Context().Done():
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
	limit := MaxRequestBytes
	if r.URL.Path == NativeEventPath {
		limit = MaxNativeEventBytes
	}
	if r.URL.Path == CommandPath {
		limit = provider.MaxCommandRequestBytes
	}
	if r.URL.Path == FileWritePath {
		limit = 2 * provider.MaxFileWriteBytes
	}
	if r.ContentLength > int64(limit) {
		writeProblem(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return false
	}
	contents, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if len(contents) > limit {
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
	case errors.Is(err, core.ErrNativeUnknown):
		writeProblem(w, 409, "native_outcome_unknown")
	case errors.Is(err, core.ErrNativeUnavailable):
		writeProblem(w, 409, "native_unavailable")
	case errors.Is(err, core.ErrInvalidEvent):
		writeProblem(w, 422, "invalid_event")
	case errors.Is(err, ErrSessionNotFound):
		writeProblem(w, http.StatusNotFound, "session_not_found")
	case errors.Is(err, core.ErrTurnNotFound):
		writeProblem(w, http.StatusNotFound, "turn_not_found")
	case errors.Is(err, core.ErrTimelineUnavailable):
		writeProblem(w, http.StatusConflict, "timeline_unavailable")
	case errors.Is(err, ErrInvalidRequest):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_request")
	case errors.Is(err, ErrSandboxNotFound):
		writeProblem(w, http.StatusNotFound, "sandbox_not_found")
	case errors.Is(err, ErrInvalidFilePath):
		writeProblem(w, http.StatusUnprocessableEntity, "invalid_file_path")
	case errors.Is(err, ErrFileNotFound):
		writeProblem(w, http.StatusNotFound, "file_not_found")
	case errors.Is(err, ErrFileTooLarge):
		writeProblem(w, http.StatusConflict, "file_too_large")
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
	if response.ContentLength > provider.MaxFileReadBytes {
		return nil, ErrFileTooLarge
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, provider.MaxFileReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read control reader response: %w", err)
	}
	if len(contents) > provider.MaxFileReadBytes {
		return nil, ErrFileTooLarge
	}
	if response.ContentLength >= 0 && response.ContentLength != int64(len(contents)) {
		return nil, fmt.Errorf("control reader returned a conflicting file length")
	}
	return contents, nil
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

func (c Client) DefaultModel(connection string) (string, error) {
	response, err := c.request(context.Background(), DefaultModelPath, connectionRequest{Connection: connection})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", decodeProblem(response)
	}
	var result modelResponse
	if err := decodeJSONResponse(response, &result, maxProblemBytes, "JSON"); err != nil {
		return "", err
	}
	if !validModel(result.Model) {
		return "", ErrUnavailable
	}
	return result.Model, nil
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
	return problemError(value.Code, response.StatusCode)
}

func problemError(code string, status int) error {
	switch code {
	case "native_outcome_unknown":
		return core.ErrNativeUnknown
	case "native_unavailable":
		return core.ErrNativeUnavailable
	case "invalid_event":
		return core.ErrInvalidEvent
	case "session_not_found":
		return ErrSessionNotFound
	case "turn_not_found":
		return core.ErrTurnNotFound
	case "timeline_unavailable":
		return core.ErrTimelineUnavailable
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
	case "file_too_large":
		return ErrFileTooLarge
	case "unavailable":
		return ErrUnavailable
	case "response_too_large":
		return ErrResponseTooLarge
	default:
		return fmt.Errorf("control reader returned HTTP %d", status)
	}
}

func problemMatchesStatus(code string, status int) bool {
	switch code {
	case "unauthorized":
		return status == http.StatusUnauthorized
	case "invalid_request":
		return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
	case "sandbox_not_found", "file_not_found", "session_not_found", "turn_not_found":
		return status == http.StatusNotFound
	case "invalid_file_path", "invalid_event":
		return status == http.StatusUnprocessableEntity
	case "native_outcome_unknown", "native_unavailable", "unavailable", "response_too_large", "timeline_unavailable", "file_too_large":
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
