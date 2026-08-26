package controlapi

import "strings"

// ProblemDescriptor is the stable, machine-readable meaning of one Dorf
// Problem code. The OpenAPI document publishes this catalog verbatim, and the
// HTTP boundary uses ProblemForCode to construct responses from the same
// authority.
type ProblemDescriptor struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
}

var problemCatalog = []ProblemDescriptor{
	describeProblem(415, "body_not_allowed", "This operation does not accept a body or Content-Type", false),
	describeProblem(413, "body_too_large", "Request body is too large", false),
	describeProblem(409, "client_conflict", "Client credential is already registered", false),
	describeProblem(401, "enrollment_unavailable", "Enrollment is invalid, expired, or already used", false),
	describeProblem(500, "evidence_unverified", "Retained Evidence could not be verified", false),
	describeProblem(404, "file_not_found", "Sandbox file not found", false),
	describeProblem(400, "file_path_required", "Exactly one path query parameter is required", false),
	describeProblem(409, "file_unavailable", "Sandbox file is unavailable", false),
	describeProblem(409, "idempotency_conflict", "Idempotency-Key is bound to different input", false),
	describeProblem(400, "idempotency_key_required", "Exactly one valid Idempotency-Key is required", false),
	describeProblem(500, "internal_error", "The request could not be completed", true),
	describeProblem(400, "invalid_cursor", "Cursor is invalid", false),
	describeProblem(422, "invalid_file_path", "Sandbox file path must be clean and workspace-relative", false),
	describeProblem(422, "invalid_input", "Input could not be accepted", false),
	describeProblem(400, "invalid_json", "Request body must be one strict JSON object", false),
	describeProblem(400, "invalid_last_event_id", "Last-Event-ID must be one exact representation identifier", false),
	describeProblem(400, "invalid_query", "Query parameters are invalid", false),
	describeProblem(404, "job_not_found", "Job not found", false),
	describeProblem(404, "message_not_found", "Message not found", false),
	describeProblem(409, "message_unavailable", "Message is unavailable for this operation", false),
	describeProblem(405, "method_not_allowed", "Method not allowed", false),
	describeProblem(406, "not_acceptable", "Accept must be text/event-stream", false),
	describeProblem(404, "not_found", "Resource not found", false),
	describeProblem(429, "rate_limited", "Too many enrollment attempts", true),
	describeProblem(409, "retry_unavailable", "The Job has no eligible failed execution to retry", false),
	describeProblem(404, "sandbox_not_found", "Sandbox not found", false),
	describeProblem(409, "steer_unavailable", "No exact active delivery can accept a steer", false),
	describeProblem(401, "unauthenticated", "A valid Client credential is required", false),
	describeProblem(415, "unsupported_media_type", "Content-Type must be application/json", false),
	describeProblem(400, "unsupported_precondition", "Conditional headers are not supported for this mutation", false),
}

func describeProblem(status int, code, title string, retryable bool) ProblemDescriptor {
	return ProblemDescriptor{
		Type:      "https://dorf.dev/problems/" + problemSlug(code),
		Title:     title,
		Status:    status,
		Code:      code,
		Retryable: retryable,
	}
}

// ProblemDescriptors returns the complete catalog in stable code order.
func ProblemDescriptors() []ProblemDescriptor {
	return append([]ProblemDescriptor(nil), problemCatalog...)
}

// ProblemForCode constructs one response from the published catalog. Details
// is deliberately present even when empty, as required by the wire contract.
func ProblemForCode(code string) (Problem, bool) {
	for _, descriptor := range problemCatalog {
		if descriptor.Code == code {
			return Problem{
				Type: descriptor.Type, Title: descriptor.Title, Status: descriptor.Status,
				Code: descriptor.Code, Retryable: descriptor.Retryable, Details: map[string]any{},
			}, true
		}
	}
	return Problem{}, false
}

func problemSlug(code string) string {
	return strings.ReplaceAll(code, "_", "-")
}
