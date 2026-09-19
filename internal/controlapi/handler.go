package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

const (
	maxBodyBytes           = 8 << 20
	maxHeaderBytes         = 32 << 10
	watchPollInterval      = time.Second
	watchKeepaliveInterval = 15 * time.Second
	watchWriteTimeout      = 10 * time.Second
	watchAuthenticationTTL = time.Minute
)

type handler struct {
	discovery Discovery
	auth      Auth
	sessions  Sessions
	profiles  Profiles
	mux       *http.ServeMux
	redeem    redemptionLimiter
	fileReads chan struct{}
	shutdown  context.Context
}

type authenticatedRoute func(http.ResponseWriter, *http.Request, controlauth.Client)

func newHandlerContext(discovery Discovery, auth Auth, sessions Sessions, profiles Profiles, shutdown context.Context) http.Handler {
	h := &handler{discovery: discovery, auth: auth, sessions: sessions, profiles: profiles, mux: http.NewServeMux(),
		fileReads: make(chan struct{}, provider.MaxConcurrentFileReads), shutdown: shutdown}
	h.mux.HandleFunc("/v1", h.discoveryRoute)
	h.mux.HandleFunc(OpenAPIPath, h.openAPIRoute)
	h.mux.HandleFunc("/v1/auth/enrollments/redeem", h.redeemRoute)
	h.mux.HandleFunc("/v1/me", h.authenticate(h.meRoute))
	h.mux.HandleFunc("/v1/profiles", h.authenticate(h.profilesRoute))
	h.mux.HandleFunc("/v1/sessions", h.authenticate(h.sessionsRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/history", h.authenticate(h.timelineRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/events", h.authenticate(h.eventsRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/workspace", h.authenticate(h.workspaceRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/turns", h.authenticate(h.turnsRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/watch", h.authenticate(h.watchRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/turns/{turn}", h.authenticate(h.turnObservationRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/events/stream", h.authenticate(h.turnObservationRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/retries", h.authenticate(h.retryRoute))
	h.mux.HandleFunc("/v1/sessions/{session}/cleanup", h.authenticate(h.cleanupRoute))
	h.mux.HandleFunc("/v1/sessions/{session}", h.authenticate(h.sessionRoute))
	h.mux.HandleFunc("/v1/sandboxes/{sandbox}/status", h.authenticate(h.sandboxStatusRoute))
	h.mux.HandleFunc("/v1/sandboxes/{sandbox}/exec", h.authenticate(h.sandboxExecRoute))
	h.mux.HandleFunc("/v1/sandboxes/{sandbox}/files", h.authenticate(h.fileRoute))
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		h.fail(w, problem("not_found"))
	})
	return h
}

func NewServer(discovery Discovery, auth Auth, sessions Sessions, profiles Profiles) *http.Server {
	discovery.Links = OpenAPIDiscoveryLinks()
	if !slices.Contains(discovery.Capabilities, OpenAPICapability) {
		discovery.Capabilities = append(discovery.Capabilities, OpenAPICapability)
	}
	shutdown, cancel := context.WithCancel(context.Background())
	server := &http.Server{Handler: newHandlerContext(discovery, auth, sessions, profiles, shutdown), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: maxHeaderBytes}
	server.RegisterOnShutdown(cancel)
	return server
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	clean := path.Clean(r.URL.Path)
	if r.URL.Path == "" || clean != r.URL.Path || strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") {
		h.fail(w, problem("not_found"))
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *handler) discoveryRoute(w http.ResponseWriter, r *http.Request) {
	if h.exact(w, r, http.MethodGet, false) {
		h.reply(w, http.StatusOK, h.discovery)
	}
}

func (h *handler) openAPIRoute(w http.ResponseWriter, r *http.Request) {
	if h.exact(w, r, http.MethodGet, false) {
		h.replyJSON(w, http.StatusOK, OpenAPIDocument())
	}
}

func (h *handler) profilesRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	profiles, err := h.profiles.List(r.Context())
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, profiles)
}

func (h *handler) redeemRoute(w http.ResponseWriter, r *http.Request) {
	if !h.exact(w, r, http.MethodPost, true) {
		return
	}
	var input RedeemRequest
	if !h.decode(w, r, &input) {
		return
	}
	if retryAfter := h.redeem.take(time.Now()); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((retryAfter+time.Second-1)/time.Second)))
		h.fail(w, problem("rate_limited"))
		return
	}
	client, created, err := h.auth.Redeem(r.Context(), input.EnrollmentCode, input.ClientName, input.Credential)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, createdStatus(created), identity(client))
}

type redemptionLimiter struct {
	mu       sync.Mutex
	window   time.Time
	attempts int
}

func (l *redemptionLimiter) take(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	const window, limit = time.Minute, 10
	if l.window.IsZero() || now.Sub(l.window) >= window {
		l.window = now
		l.attempts = 0
	}
	if l.attempts >= limit {
		return window - now.Sub(l.window)
	}
	l.attempts++
	return 0
}

func (h *handler) authenticate(route authenticatedRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			h.authError(w)
			return
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			h.authError(w)
			return
		}
		client, err := h.auth.Authenticate(r.Context(), parts[1])
		if err != nil {
			h.serviceError(w, r, err)
			return
		}
		route(w, r, client)
	}
}

func (h *handler) meRoute(w http.ResponseWriter, r *http.Request, client controlauth.Client) {
	if h.exact(w, r, http.MethodGet, false) {
		h.reply(w, http.StatusOK, identity(client))
	}
}

func (h *handler) sessionsRoute(w http.ResponseWriter, r *http.Request, client controlauth.Client) {
	switch r.Method {
	case http.MethodGet:
		h.sessionListRoute(w, r)
	case http.MethodPost:
		h.createSessionRoute(w, r, client)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		h.fail(w, problem("method_not_allowed"))
	}
}

func (h *handler) createSessionRoute(w http.ResponseWriter, r *http.Request, client controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, true) {
		return
	}
	key, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	var input CreateSessionRequest
	if !h.decode(w, r, &input) {
		return
	}
	session, created, err := h.sessions.Create(r.Context(), client.ID, key, input)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.sessionResponseStatus(w, r, session, nil, createdStatus(created))
}

func (h *handler) retryRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if !h.exact(w, r, http.MethodPost, false) {
		return
	}
	key, ok := h.idempotencyKey(w, r)
	if !ok {
		return
	}
	retry, created, err := h.sessions.Retry(r.Context(), r.PathValue("session"), key)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, createdStatus(created), retry)
}

func (h *handler) cleanupRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if h.exact(w, r, http.MethodPut, false) {
		session, err := h.sessions.RequestCleanup(r.Context(), r.PathValue("session"))
		h.sessionResponse(w, r, session, err)
	}
}

func (h *handler) sessionRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if h.exact(w, r, http.MethodGet, false) {
		session, err := h.sessions.Get(r.Context(), r.PathValue("session"))
		h.sessionResponse(w, r, session, err)
	}
}

func (h *handler) sessionListRoute(w http.ResponseWriter, r *http.Request) {
	if contentTypes := r.Header.Values("Content-Type"); len(contentTypes) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		h.fail(w, problem("body_not_allowed"))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		h.fail(w, problem("invalid_query"))
		return
	}
	for key := range query {
		if key != "limit" && key != "cursor" {
			h.fail(w, problem("invalid_query"))
			return
		}
	}
	limit := 50
	if values, found := query["limit"]; found {
		if len(values) != 1 {
			h.fail(w, problem("invalid_query"))
			return
		}
		parsed, err := strconv.ParseUint(values[0], 10, 8)
		if err != nil || parsed < 1 || parsed > 100 {
			h.fail(w, problem("invalid_query"))
			return
		}
		limit = int(parsed)
	}
	cursor := ""
	if values, found := query["cursor"]; found {
		if len(values) != 1 {
			h.fail(w, problem("invalid_query"))
			return
		}
		cursor = values[0]
		if cursor == "" {
			h.fail(w, problem("invalid_cursor"))
			return
		}
	}
	list, err := h.sessions.List(r.Context(), limit, cursor)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	if list.Sessions == nil {
		list.Sessions = []SessionSummary{}
	}
	h.reply(w, http.StatusOK, list)
}

func (h *handler) sessionResponse(w http.ResponseWriter, r *http.Request, session Session, err error) {
	h.sessionResponseStatus(w, r, session, err, http.StatusOK)
}

func (h *handler) sessionResponseStatus(w http.ResponseWriter, r *http.Request, session Session, err error, status int) {
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	body, id, err := sessionRepresentation(session)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	etag := `"` + id + `"`
	w.Header().Set("ETag", etag)
	if r.Method == http.MethodGet && ifNoneMatch(r.Header.Values("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.replyJSON(w, status, body)
}

func (h *handler) exact(w http.ResponseWriter, r *http.Request, method string, hasJSON bool) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		h.fail(w, problem("method_not_allowed"))
		return false
	}
	if r.URL.RawQuery != "" {
		h.fail(w, problem("invalid_query"))
		return false
	}
	if method != http.MethodGet && hasConditionalHeader(r) {
		h.fail(w, problem("unsupported_precondition"))
		return false
	}
	contentTypes := r.Header.Values("Content-Type")
	if hasJSON && (len(contentTypes) != 1 || contentTypes[0] != "application/json") {
		h.fail(w, problem("unsupported_media_type"))
		return false
	}
	if !hasJSON && (len(contentTypes) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0) {
		h.fail(w, problem("body_not_allowed"))
		return false
	}
	return true
}

func (h *handler) idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || keys[0] != strings.TrimSpace(keys[0]) || keys[0] == "" || len(keys[0]) > 255 {
		h.fail(w, problem("idempotency_key_required"))
		return "", false
	}
	return keys[0], true
}

func (h *handler) watchRoute(w http.ResponseWriter, r *http.Request, client controlauth.Client) {
	if !h.exact(w, r, http.MethodGet, false) {
		return
	}
	accept := r.Header.Values("Accept")
	if len(accept) != 1 || accept[0] != "text/event-stream" {
		h.fail(w, problem("not_acceptable"))
		return
	}
	lastIDs := r.Header.Values("Last-Event-ID")
	if len(lastIDs) > 1 || len(lastIDs) == 1 && lastIDs[0] != strings.TrimSpace(lastIDs[0]) {
		h.fail(w, problem("invalid_last_event_id"))
		return
	}
	lastID := ""
	if len(lastIDs) == 1 {
		lastID = lastIDs[0]
		decoded, err := hex.DecodeString(lastID)
		if err != nil || len(decoded) != sha256.Size {
			h.fail(w, problem("invalid_last_event_id"))
			return
		}
	}
	authenticationDeadline := streamAuthenticationDeadline(client)
	ctx, cancel := context.WithDeadline(r.Context(), authenticationDeadline)
	stopShutdown := context.AfterFunc(h.shutdown, cancel)
	defer stopShutdown()
	defer cancel()

	controller := http.NewResponseController(w)
	session, err := h.sessions.Get(ctx, r.PathValue("session"))
	if err != nil {
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil {
				h.fail(w, problem("unauthenticated"))
			}
			return
		}
		h.serviceError(w, r, err)
		return
	}
	body, currentID, err := sessionRepresentation(session)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	if err := controller.SetWriteDeadline(time.Now().Add(watchWriteTimeout)); err != nil {
		h.serviceError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
	if lastID != currentID {
		if err := writeSnapshot(w, controller, currentID, body); err != nil {
			return
		}
		lastID = currentID
	} else if err := streamWrite(controller, controller.Flush); err != nil {
		return
	}

	poll := time.NewTicker(watchPollInterval)
	keepalive := time.NewTicker(watchKeepaliveInterval)
	defer poll.Stop()
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			if err := streamWrite(controller, func() error {
				if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
					return err
				}
				return controller.Flush()
			}); err != nil {
				return
			}
		case <-poll.C:
			session, err := h.sessions.Get(ctx, r.PathValue("session"))
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("Dorf control API watch stopped: path=%q error_type=%T", r.URL.Path, err)
				return
			}
			body, currentID, err := sessionRepresentation(session)
			if err != nil {
				log.Printf("Dorf control API watch stopped: path=%q error_type=%T", r.URL.Path, err)
				return
			}
			if currentID == lastID {
				continue
			}
			if err := writeSnapshot(w, controller, currentID, body); err != nil {
				return
			}
			lastID = currentID
		}
	}
}

func writeSnapshot(w io.Writer, controller *http.ResponseController, id string, body []byte) error {
	return streamWrite(controller, func() error {
		if _, err := io.WriteString(w, "event: snapshot\nid: "+id+"\ndata: "); err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
		if _, err := io.WriteString(w, "\n\n"); err != nil {
			return err
		}
		return controller.Flush()
	})
}

func streamWrite(controller *http.ResponseController, write func() error) error {
	if err := controller.SetWriteDeadline(time.Now().Add(watchWriteTimeout)); err != nil {
		return err
	}
	writeErr := write()
	clearErr := controller.SetWriteDeadline(time.Time{})
	if writeErr != nil {
		return writeErr
	}
	return clearErr
}

func (h *handler) fileRoute(w http.ResponseWriter, r *http.Request, _ controlauth.Client) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		h.fail(w, problem("method_not_allowed"))
		return
	}
	if contentTypes := r.Header.Values("Content-Type"); r.Method == http.MethodGet && (len(contentTypes) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0) {
		h.fail(w, problem("body_not_allowed"))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		h.fail(w, problem("invalid_query"))
		return
	}
	paths, found := query["path"]
	if !found || len(paths) == 0 || paths[0] == "" {
		h.fail(w, problem("file_path_required"))
		return
	}
	if len(query) != 1 || len(paths) != 1 {
		h.fail(w, problem("invalid_query"))
		return
	}
	if r.Method == http.MethodPut {
		h.writeFile(w, r, paths[0])
		return
	}
	h.readFile(w, r, paths[0])
}

func (h *handler) readFile(w http.ResponseWriter, r *http.Request, name string) {
	select {
	case h.fileReads <- struct{}{}:
		defer func() { <-h.fileReads }()
	case <-r.Context().Done():
		return
	}
	contents, err := h.sessions.ReadSandboxFile(r.Context(), r.PathValue("sandbox"), name)
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	if len(contents) > provider.MaxFileReadBytes {
		h.serviceError(w, r, ErrFileTooLarge)
		return
	}
	digest := sha256.Sum256(contents)
	w.Header().Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(digest[:])+":")
	w.Header().Set("Content-Length", strconv.Itoa(len(contents)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contents)
}

func sessionRepresentation(session Session) ([]byte, string, error) {
	if session.ID == "" {
		return nil, "", fmt.Errorf("control API Session representation has invalid identity")
	}
	body, err := json.Marshal(session)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(body)
	return body, hex.EncodeToString(digest[:]), nil
}

func ifNoneMatch(values []string, etag string) bool {
	target := strings.TrimPrefix(etag, "W/")
	for _, value := range values {
		for candidate := range strings.SplitSeq(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == target {
				return true
			}
		}
	}
	return false
}

func hasConditionalHeader(r *http.Request) bool {
	for _, name := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range"} {
		if len(r.Header.Values(name)) != 0 {
			return true
		}
	}
	return false
}

func (h *handler) decode(w http.ResponseWriter, r *http.Request, output any) bool {
	limit := maxBodyBytes
	if strings.HasSuffix(r.URL.Path, "/events") {
		limit = 46 << 20
	}
	if r.ContentLength > int64(limit) {
		h.fail(w, problem("body_too_large"))
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(limit)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.fail(w, problem("body_too_large"))
		} else {
			h.fail(w, problem("invalid_json"))
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		h.fail(w, problem("invalid_json"))
		return false
	}
	return true
}

func (h *handler) serviceError(w http.ResponseWriter, r *http.Request, err error) {
	if code := attachmentProblemCode(err); code != "" {
		h.fail(w, problem(code))
		return
	}
	if code := admissionProblemCode(err); code != "" {
		h.fail(w, problem(code))
		return
	}
	if errors.Is(err, controlauth.ErrUnauthenticated) {
		h.authError(w)
		return
	}
	for _, entry := range serviceProblems {
		if errors.Is(err, entry.err) {
			h.fail(w, problem(entry.code))
			return
		}
	}
	log.Printf("Dorf control API internal failure: method=%s path=%q error_type=%T", r.Method, r.URL.Path, err)
	h.fail(w, problem("internal_error"))
}

var serviceProblems = []struct {
	err  error
	code string
}{
	{controlauth.ErrEnrollmentUnavailable, "enrollment_unavailable"},
	{controlauth.ErrClientConflict, "client_conflict"},
	{ErrInvalidCursor, "invalid_cursor"},
	{ErrSessionNotFound, "session_not_found"},
	{ErrTurnNotFound, "turn_not_found"},
	{ErrTimelineUnavailable, "timeline_unavailable"},
	{ErrSandboxStatusUnavailable, "sandbox_status_unavailable"},
	{ErrSandboxExecUnavailable, "sandbox_exec_unavailable"},
	{ErrSandboxExecFailed, "sandbox_exec_failed"},
	{ErrSandboxNotFound, "sandbox_not_found"},
	{ErrInvalidFilePath, "invalid_file_path"},
	{ErrFileNotFound, "file_not_found"},
	{ErrFileTooLarge, "file_too_large"},
	{ErrFileUnavailable, "file_unavailable"},
	{ErrRetryUnavailable, "retry_unavailable"},
}

func admissionProblemCode(err error) string {
	switch {
	case errors.Is(err, core.ErrNativeUnknown):
		return "native_outcome_unknown"
	case errors.Is(err, core.ErrNativeUnavailable):
		return "native_unavailable"
	case errors.Is(err, core.ErrInvalidEvent):
		return "invalid_input"
	case errors.Is(err, ErrProfileNotFound):
		return "profile_not_found"
	case errors.Is(err, controlauth.ErrInvalidInput), errors.Is(err, ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, ErrIdempotencyConflict):
		return "idempotency_conflict"
	default:
		return ""
	}
}

func attachmentProblemCode(err error) string {
	switch {
	case errors.Is(err, ErrAttachmentAnimationUnsupported):
		return "attachment_animation_unsupported"
	case errors.Is(err, ErrAttachmentImageTooLarge):
		return "attachment_image_too_large"
	default:
		return ""
	}
}

func (h *handler) authError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="dorf"`)
	h.fail(w, problem("unauthenticated"))
}

func (h *handler) reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *handler) replyJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *handler) fail(w http.ResponseWriter, value Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(value.Status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(code string) Problem {
	value, found := ProblemForCode(code)
	if !found {
		panic(fmt.Sprintf("unknown control API Problem %q", code))
	}
	return value
}

func identity(client controlauth.Client) Identity {
	return Identity{Principal: Principal{ID: controlauth.DeploymentOperatorPrincipalID, Name: "Deployment operator"}, Client: Client{ID: client.ID, Name: client.Name, ExpiresAt: credentialExpiry(client.CredentialExpiresAt)}}
}

func createdStatus(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}

func credentialExpiry(expiry time.Time) *time.Time {
	if expiry.IsZero() {
		return nil
	}
	return &expiry
}

func streamAuthenticationDeadline(client controlauth.Client) time.Time {
	deadline := time.Now().Add(watchAuthenticationTTL)
	if !client.CredentialExpiresAt.IsZero() && client.CredentialExpiresAt.Before(deadline) {
		return client.CredentialExpiresAt
	}
	return deadline
}
