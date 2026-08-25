package controlapi

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
)

const (
	maxBodyBytes   = 8 << 20
	maxHeaderBytes = 32 << 10
)

type handler struct {
	discovery Discovery
	auth      Auth
	jobs      Jobs
	mux       *http.ServeMux
	redeem    redemptionLimiter
}

func newHandler(discovery Discovery, auth Auth, jobs Jobs) http.Handler {
	h := &handler{discovery: discovery, auth: auth, jobs: jobs, mux: http.NewServeMux()}
	h.mux.HandleFunc("/v1", h.discoveryRoute)
	h.mux.HandleFunc("/v1/auth/enrollments/redeem", h.redeemRoute)
	h.mux.HandleFunc("/v1/me", h.protectedRoute)
	h.mux.HandleFunc("/v1/jobs", h.protectedRoute)
	h.mux.HandleFunc("/v1/jobs/{job}/cleanup", h.protectedRoute)
	h.mux.HandleFunc("/v1/jobs/{job}", h.protectedRoute)
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		h.fail(w, problem(http.StatusNotFound, "not_found", "Resource not found"))
	})
	return h
}

func NewServer(discovery Discovery, auth Auth, jobs Jobs) *http.Server {
	return &http.Server{Handler: newHandler(discovery, auth, jobs), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: maxHeaderBytes}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	clean := path.Clean(r.URL.Path)
	if r.URL.RawQuery != "" || r.URL.Path == "" || clean != r.URL.Path || strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") {
		h.fail(w, problem(http.StatusNotFound, "not_found", "Resource not found"))
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *handler) discoveryRoute(w http.ResponseWriter, r *http.Request) {
	if h.exact(w, r, http.MethodGet, false) {
		h.reply(w, http.StatusOK, h.discovery)
	}
}

func (h *handler) redeemRoute(w http.ResponseWriter, r *http.Request) {
	if !h.exact(w, r, http.MethodPost, true) {
		return
	}
	var input RedeemRequest
	if !h.decode(w, r, &input) {
		return
	}
	if retryAfter := h.redeem.take(redemptionKey(input.EnrollmentCode), time.Now()); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((retryAfter+time.Second-1)/time.Second)))
		value := problem(http.StatusTooManyRequests, "rate_limited", "Too many enrollment attempts")
		value.Retryable = true
		h.fail(w, value)
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
	mu      sync.Mutex
	buckets map[string]redemptionBucket
}

type redemptionBucket struct {
	window   time.Time
	attempts int
}

func (l *redemptionLimiter) take(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	const window, limit, maxBuckets = time.Minute, 10, 1024
	if l.buckets == nil {
		l.buckets = make(map[string]redemptionBucket)
	}
	bucket := l.buckets[key]
	if bucket.window.IsZero() || now.Sub(bucket.window) >= window {
		bucket = redemptionBucket{window: now}
	}
	if bucket.attempts >= limit {
		return window - now.Sub(bucket.window)
	}
	if _, exists := l.buckets[key]; !exists && len(l.buckets) >= maxBuckets {
		for candidate, value := range l.buckets {
			if now.Sub(value.window) >= window {
				delete(l.buckets, candidate)
			}
		}
		if len(l.buckets) >= maxBuckets {
			for candidate := range l.buckets {
				delete(l.buckets, candidate)
				break
			}
		}
	}
	bucket.attempts++
	l.buckets[key] = bucket
	return 0
}

func redemptionKey(token string) string {
	id, _, found := strings.Cut(token, ".")
	if !found || len(id) != len("enr_")+22 || !strings.HasPrefix(id, "enr_") {
		return "invalid"
	}
	return id
}

func (h *handler) protectedRoute(w http.ResponseWriter, r *http.Request) {
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

	switch r.Pattern {
	case "/v1/me":
		if h.exact(w, r, http.MethodGet, false) {
			h.reply(w, http.StatusOK, identity(client))
		}
	case "/v1/jobs":
		if !h.exact(w, r, http.MethodPost, true) {
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 || keys[0] != strings.TrimSpace(keys[0]) || keys[0] == "" || len(keys[0]) > 255 {
			h.fail(w, problem(http.StatusBadRequest, "idempotency_key_required", "Exactly one valid Idempotency-Key is required"))
			return
		}
		var input AdmitJobRequest
		if !h.decode(w, r, &input) {
			return
		}
		job, created, err := h.jobs.AdmitDirect(r.Context(), keys[0], input)
		if err != nil {
			h.serviceError(w, r, err)
			return
		}
		h.reply(w, createdStatus(created), job)
	case "/v1/jobs/{job}/cleanup":
		if h.exact(w, r, http.MethodPut, false) {
			job, err := h.jobs.RequestCleanup(r.Context(), r.PathValue("job"))
			h.jobResponse(w, r, job, err)
		}
	default:
		if h.exact(w, r, http.MethodGet, false) {
			job, err := h.jobs.Get(r.Context(), r.PathValue("job"))
			h.jobResponse(w, r, job, err)
		}
	}
}

func (h *handler) jobResponse(w http.ResponseWriter, r *http.Request, job Job, err error) {
	if err != nil {
		h.serviceError(w, r, err)
		return
	}
	h.reply(w, http.StatusOK, job)
}

func (h *handler) exact(w http.ResponseWriter, r *http.Request, method string, hasJSON bool) bool {
	if r.Method != method {
		w.Header().Set("Allow", method)
		h.fail(w, problem(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed"))
		return false
	}
	contentTypes := r.Header.Values("Content-Type")
	if hasJSON && (len(contentTypes) != 1 || contentTypes[0] != "application/json") {
		h.fail(w, problem(http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json"))
		return false
	}
	if !hasJSON && (len(contentTypes) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0) {
		h.fail(w, problem(http.StatusUnsupportedMediaType, "body_not_allowed", "This operation does not accept a body or Content-Type"))
		return false
	}
	return true
}

func (h *handler) decode(w http.ResponseWriter, r *http.Request, output any) bool {
	if r.ContentLength > maxBodyBytes {
		h.fail(w, problem(http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large"))
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.fail(w, problem(http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large"))
		} else {
			h.fail(w, problem(http.StatusBadRequest, "invalid_json", "Request body must be one strict JSON object"))
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		h.fail(w, problem(http.StatusBadRequest, "invalid_json", "Request body must contain one JSON object"))
		return false
	}
	return true
}

func (h *handler) serviceError(w http.ResponseWriter, r *http.Request, err error) {
	var value Problem
	switch {
	case errors.Is(err, controlauth.ErrUnauthenticated):
		h.authError(w)
		return
	case errors.Is(err, controlauth.ErrEnrollmentUnavailable):
		value = problem(http.StatusUnauthorized, "enrollment_unavailable", "Enrollment is invalid, expired, or already used")
	case errors.Is(err, controlauth.ErrClientConflict):
		value = problem(http.StatusConflict, "client_conflict", "Enrollment is bound to different Client input")
	case errors.Is(err, controlauth.ErrInvalidInput), errors.Is(err, ErrInvalidInput):
		value = problem(http.StatusUnprocessableEntity, "invalid_input", "Input could not be accepted")
	case errors.Is(err, ErrJobNotFound):
		value = problem(http.StatusNotFound, "job_not_found", "Job not found")
	case errors.Is(err, ErrIdempotencyConflict):
		value = problem(http.StatusConflict, "idempotency_conflict", "Idempotency-Key is bound to different Job input")
	default:
		log.Printf("Dorf control API internal failure: method=%s path=%q error_type=%T", r.Method, r.URL.Path, err)
		value = problem(http.StatusInternalServerError, "internal_error", "The request could not be completed")
		value.Retryable = true
	}
	h.fail(w, value)
}

func (h *handler) authError(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="dorf"`)
	h.fail(w, problem(http.StatusUnauthorized, "unauthenticated", "A valid Client credential is required"))
}

func (h *handler) reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *handler) fail(w http.ResponseWriter, value Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(value.Status)
	_ = json.NewEncoder(w).Encode(value)
}

func problem(status int, code, title string) Problem {
	return Problem{Type: "https://dorf.dev/problems/" + strings.ReplaceAll(code, "_", "-"), Title: title,
		Status: status, Code: code, Details: map[string]any{}}
}

func identity(client controlauth.Client) Identity {
	return Identity{Principal: Principal{ID: controlauth.DeploymentOperatorPrincipalID, Name: "Deployment operator"}, Client: Client{ID: client.ID, Name: client.Name, ExpiresAt: client.CredentialExpiresAt}}
}

func createdStatus(created bool) int {
	if created {
		return http.StatusCreated
	}
	return http.StatusOK
}
