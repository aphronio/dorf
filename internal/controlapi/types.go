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
	ErrMessageNotFound     = errors.New("control API Message not found")
	ErrSandboxNotFound     = errors.New("control API Sandbox not found")
	ErrInvalidFilePath     = errors.New("control API Sandbox file path invalid")
	ErrFileNotFound        = errors.New("control API Sandbox file not found")
	ErrFileUnavailable     = errors.New("control API Sandbox file unavailable")
	ErrMessageUnavailable  = errors.New("control API Message cannot be accepted")
	ErrSteerUnavailable    = errors.New("control API steer cannot be accepted")
	ErrRetryUnavailable    = errors.New("control API Job retry unavailable")
	ErrEvidenceUnverified  = errors.New("control API Evidence could not be verified")
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
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	Goal             string     `json:"goal"`
	Profile          string     `json:"profile"`
	Model            string     `json:"model"`
	Reasoning        string     `json:"reasoning"`
	InitialMessageID string     `json:"initial_message_id"`
	Admission        Admission  `json:"admission"`
	Execution        State      `json:"execution"`
	Attention        *Attention `json:"attention"`
	Cleanup          State      `json:"cleanup"`
	Sandboxes        []Sandbox  `json:"sandboxes"`
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

type SendMessageRequest struct {
	Text   string `json:"text"`
	Intent string `json:"intent"`
}

// Message projects one accepted delivery without exposing its Harness Thread,
// Turn, or internal AgentRun identity.
type Message struct {
	ID         string         `json:"id"`
	JobID      string         `json:"job_id"`
	Sequence   int64          `json:"sequence"`
	Intent     string         `json:"intent"`
	Delivery   State          `json:"delivery"`
	Result     *MessageResult `json:"result"`
	Attention  *Attention     `json:"attention"`
	AdmittedAt time.Time      `json:"admitted_at"`
}

type MessageResult struct {
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

// Retry acknowledges one caller-keyed request against the Job's existing
// execution authority. Internal task and run identities remain private.
type Retry struct {
	JobID string `json:"job_id"`
	State string `json:"state"`
}

type EvidenceList struct {
	Evidence []Evidence `json:"evidence"`
}

// Evidence is verified metadata only. The retained bytes and internal
// execution-owner identities are not part of this endpoint.
type Evidence struct {
	ID         string    `json:"id"`
	SHA256     string    `json:"sha256"`
	ByteSize   int64     `json:"byte_size"`
	MediaType  string    `json:"media_type"`
	Producer   string    `json:"producer"`
	Kind       string    `json:"kind"`
	Revision   string    `json:"revision,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitempty,omitzero"`
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
	SendMessage(context.Context, string, string, SendMessageRequest) (Message, bool, error)
	GetMessage(context.Context, string, string) (Message, error)
	Retry(context.Context, string, string) (Retry, bool, error)
	ReadSandboxFile(context.Context, string, string) ([]byte, error)
	Evidence(context.Context, string) ([]Evidence, error)
	RequestCleanup(context.Context, string) (Job, error)
}
