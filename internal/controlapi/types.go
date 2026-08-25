// Package controlapi owns the stable HTTP representation of Dorf's remote
// control boundary. It deliberately does not serialize Core or persistence
// records directly.
package controlapi

import (
	"context"
	"errors"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
)

var (
	ErrInvalidInput        = errors.New("invalid control API input")
	ErrJobNotFound         = errors.New("control API Job not found")
	ErrIdempotencyConflict = errors.New("idempotency key is bound to different input")
)

type Discovery struct {
	Product      string   `json:"product"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

type Principal struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Client struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Identity struct {
	Principal Principal `json:"principal"`
	Client    Client    `json:"client"`
}

type RedeemRequest struct {
	EnrollmentCode string `json:"enrollment_code"`
	ClientName     string `json:"client_name"`
	Credential     string `json:"credential"`
}

type AdmitJobRequest struct {
	Goal      string `json:"goal"`
	Profile   string `json:"profile"`
	Model     string `json:"model"`
	Reasoning string `json:"reasoning"`
}

type Job struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Goal      string     `json:"goal"`
	Profile   string     `json:"profile"`
	Model     string     `json:"model"`
	Reasoning string     `json:"reasoning"`
	Admission Admission  `json:"admission"`
	Execution State      `json:"execution"`
	Attention *Attention `json:"attention"`
	Outcome   *struct{}  `json:"outcome"`
	Cleanup   State      `json:"cleanup"`
	Sandboxes []Sandbox  `json:"sandboxes"`
}

type Admission struct {
	Open bool `json:"open"`
}

type State struct {
	State string `json:"state"`
}

type Attention struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type Sandbox struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Problem is RFC 9457 Problem Details extended with stable Dorf recovery
// fields. Details is always encoded, including when it is empty.
type Problem struct {
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Status    int            `json:"status"`
	Code      string         `json:"code"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

// Auth is the deliberately small portion of controlauth.Service used by HTTP.
// Enrollment accepts the client credential once; no response contains it.
type Auth interface {
	Authenticate(context.Context, string) (controlauth.Client, error)
	Redeem(context.Context, string, string, string) (controlauth.Client, bool, error)
}

// Jobs keeps domain admission, projection, and cleanup policy outside HTTP.
// Implementations compose direct.Admit and Core handles and return only the
// purpose-built public snapshot.
type Jobs interface {
	AdmitDirect(context.Context, string, AdmitJobRequest) (Job, bool, error)
	Get(context.Context, string) (Job, error)
	RequestCleanup(context.Context, string) (Job, error)
}
