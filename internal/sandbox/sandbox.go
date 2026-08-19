// Package sandbox defines the provider-neutral execution boundary consumed by
// Dorf's Harnesses and workflow-facing external services.
package sandbox

import (
	"context"
	"fmt"
	"net/http"
)

// Ownership is Dorf's durable identity for one provider Sandbox. A provider's
// opaque resource locator is deliberately adapter-private.
type Ownership struct {
	JobID          string `json:"job_id"`
	SandboxID      string `json:"sandbox_id"`
	OwnershipNonce string `json:"ownership_nonce"`
}

// ReviewMetadata binds an isolated review Sandbox to the exact admitted run
// and Revision. It is additional attestation, never cleanup identity.
type ReviewMetadata struct {
	JobID          string `json:"job_id"`
	AgentRunID     string `json:"agent_run_id"`
	Revision       string `json:"revision"`
	OwnershipNonce string `json:"ownership_nonce"`
}

// Result preserves the bounded command observation used by common consumers.
// A nonzero process exit is represented in ExitCode, not as a transport error.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Endpoint separates the address a process binds inside a Sandbox from the
// provider address Dorf dials. Provider traffic capabilities remain private.
type Endpoint struct {
	ListenURL string
	DialURL   string
	headers   http.Header
}

func NewEndpoint(listenURL, dialURL string, headers http.Header) Endpoint {
	return Endpoint{ListenURL: listenURL, DialURL: dialURL, headers: headers.Clone()}
}

func (e Endpoint) Headers() http.Header { return e.headers.Clone() }

func (e Endpoint) String() string {
	return fmt.Sprintf("Sandbox endpoint %s -> %s (provider capabilities redacted)", e.ListenURL, e.DialURL)
}

func (e Endpoint) GoString() string { return e.String() }

// Sandbox is the exact infrastructure contract earned by the Incus and E2B
// proofs. Implementations own provider discovery, opaque locators, lifecycle
// APIs, command transports, endpoint routing, and topology validation.
type Sandbox interface {
	Workspace() string
	ReconcileOwnedCreate(context.Context, Ownership) error
	AttestOwnership(context.Context, Ownership) error
	AttachReviewMetadata(context.Context, Ownership, ReviewMetadata) error
	OwnedPresent(context.Context, Ownership) (bool, error)
	DeleteOwned(context.Context, Ownership) error
	AttestReview(context.Context, Ownership, ReviewMetadata) error
	ReconcileClone(context.Context, Ownership, string, string, string) error
	PutFile(context.Context, Ownership, string, []byte) error
	Exec(context.Context, Ownership, []byte, ...string) (Result, error)
	Endpoint(context.Context, Ownership, int) (Endpoint, error)
	ProviderRouteURL(context.Context, string) (string, error)
}

type OwnershipError struct{ Reason string }

func (e *OwnershipError) Error() string         { return e.Reason }
func (e *OwnershipError) AttentionNeeded() bool { return true }

func OwnershipErrorf(format string, args ...any) error {
	return &OwnershipError{Reason: fmt.Sprintf(format, args...)}
}

type UnsupportedError struct{ Capability string }

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("Sandbox provider does not support admitted capability %s", e.Capability)
}

func (e *UnsupportedError) AttentionNeeded() bool { return true }
