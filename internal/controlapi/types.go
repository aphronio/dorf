// Package controlapi owns the stable HTTP representation of Dorf's remote
// control boundary. It deliberately does not serialize Core or persistence
// records directly.
package controlapi

import (
	"context"
	"errors"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

var (
	ErrAttachmentAnimationUnsupported = errors.New("animated attachment images are unsupported")
	ErrAttachmentImageTooLarge        = errors.New("attachment image exceeds the decoded pixel limit")
	ErrInvalidInput                   = errors.New("invalid control API input")
	ErrProfileNotFound                = errors.New("control API Sandbox profile not found")
	ErrInvalidCursor                  = errors.New("invalid control API Session cursor")
	ErrSessionNotFound                = errors.New("control API Session not found")
	ErrMessageNotFound                = errors.New("control API Message not found")
	ErrSandboxStatusUnavailable       = errors.New("Sandbox status is unavailable")
	ErrSandboxExecUnavailable         = errors.New("Sandbox command is unavailable")
	ErrSandboxExecFailed              = errors.New("Sandbox command outcome is unknown")
	ErrSandboxNotFound                = errors.New("control API Sandbox not found")
	ErrInvalidFilePath                = errors.New("control API Sandbox file path invalid")
	ErrFileNotFound                   = errors.New("control API Sandbox file not found")
	ErrFileTooLarge                   = errors.New("control API Sandbox file exceeds read limit")
	ErrFileUnavailable                = errors.New("control API Sandbox file unavailable")
	ErrMessageUnavailable             = errors.New("control API Message cannot be accepted")
	ErrSteerUnavailable               = errors.New("control API steer cannot be accepted")
	ErrSkillRefreshUnavailable        = errors.New("control API skill refresh is unsupported for this profile")
	ErrMessageImageUnsupported        = errors.New("control API image attachments are unsupported for this profile")
	ErrInterruptUnavailable           = errors.New("control API interrupt cannot be accepted")
	ErrRetryUnavailable               = errors.New("control API Session retry unavailable")
	ErrIdempotencyConflict            = errors.New("idempotency key is bound to different input")
)

type Discovery struct {
	Product      string         `json:"product"`
	Version      string         `json:"version"`
	Capabilities []string       `json:"capabilities"`
	Links        DiscoveryLinks `json:"links"`
}

type Principal struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Client struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type Identity struct {
	Principal Principal `json:"principal"`
	Client    Client    `json:"client"`
}

type ProfileSummary struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Harness  string `json:"harness"`
	Default  bool   `json:"default"`
	Verified bool   `json:"verified"`
}

type ProfileList struct {
	Profiles []ProfileSummary `json:"profiles"`
}

type Profiles interface {
	List(context.Context) (ProfileList, error)
}

type RedeemRequest struct {
	EnrollmentCode string `json:"enrollment_code"`
	ClientName     string `json:"client_name"`
	Credential     string `json:"credential"`
}

type CreateSessionRequest struct {
	KeepRunning     bool   `json:"keep_running,omitempty"`
	ClientReference string `json:"client_reference,omitempty"`
	AgentsMD        string `json:"agents_md,omitempty"`
	Profile         string `json:"profile"`
	AIConnection    string `json:"ai_connection,omitempty"`
	Model           string `json:"model,omitempty"`
	Reasoning       string `json:"reasoning,omitempty"`
}

type SessionCreator struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SessionSummary is the deliberately narrow representation returned by the Session
// index. Mutable execution and cleanup state belong to canonical Session reads.
type SessionSummary struct {
	CreatedByClient *SessionCreator `json:"created_by_client"`
	ClientReference string          `json:"client_reference"`
	ID              string          `json:"id"`
	AdmittedAt      time.Time       `json:"admitted_at"`
}

type SessionList struct {
	Sessions   []SessionSummary `json:"sessions"`
	NextCursor *string          `json:"next_cursor"`
}

// Session is the canonical public execution context.
type Session struct {
	KeepRunning     bool            `json:"keep_running"`
	LatestReplyID   string          `json:"latest_reply_id,omitempty"`
	CreatedByClient *SessionCreator `json:"created_by_client"`
	ClientReference string          `json:"client_reference"`
	ID              string          `json:"id"`
	Profile         string          `json:"profile"`
	Model           string          `json:"model"`
	Reasoning       string          `json:"reasoning"`
	Admission       Admission       `json:"admission"`
	Execution       State           `json:"execution"`
	Attention       *Attention      `json:"attention"`
	Cleanup         State           `json:"cleanup"`
	Sandboxes       []Sandbox       `json:"sandboxes"`
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
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	ResourceID   string               `json:"resource_id,omitempty"`
	ProviderID   string               `json:"provider_id,omitempty"`
	Resources    []SandboxResource    `json:"resources,omitempty"`
	Upgrades     []SandboxUpgrade     `json:"upgrades,omitempty"`
	DeliveryHold *SandboxDeliveryHold `json:"delivery_hold,omitempty"`
}

type SandboxUpgrade struct {
	ID                    string     `json:"id"`
	Status                string     `json:"status"`
	SourceResourceID      string     `json:"source_resource_id"`
	DestinationResourceID string     `json:"destination_resource_id,omitempty"`
	PackageVersion        string     `json:"package_version"`
	PreviousVersion       string     `json:"previous_version,omitempty"`
	RequestedAt           time.Time  `json:"requested_at"`
	VerifiedAt            *time.Time `json:"verified_at,omitempty"`
	FinishedAt            *time.Time `json:"finished_at,omitempty"`
	Outcome               string     `json:"outcome,omitempty"`
	FailureCode           string     `json:"failure_code,omitempty"`
	CheckpointReference   string     `json:"checkpoint_reference,omitempty"`
}

type SandboxDeliveryHold struct {
	ID          string    `json:"id"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requested_at"`
}

// SandboxResource is retained infrastructure history, without ownership secrets.
type SandboxResource struct {
	ID         string     `json:"id"`
	ProviderID string     `json:"provider_id,omitempty"`
	ReservedAt time.Time  `json:"reserved_at"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`
}

type SendMessageRequest struct {
	Observation           bool                    `json:"observation,omitempty"`
	DeveloperInstructions *string                 `json:"developer_instructions,omitempty"`
	RefreshSkills         bool                    `json:"refresh_skills,omitempty"`
	Text                  string                  `json:"text"`
	Intent                string                  `json:"intent,omitempty"`
	Attachments           []SendMessageAttachment `json:"-"`
}

// SendMessageAttachment is one caller-supplied filename and byte body. The
// server derives media type, image kind, digest, and size before Core sees it.
type SendMessageAttachment struct {
	Filename string
	Contents []byte
}

// Message projects one accepted delivery without exposing its Harness Thread,
// Turn, or internal AgentRun identity.
type Message struct {
	WaitReason         string         `json:"wait_reason,omitempty"`
	InterruptRequested bool           `json:"interrupt_requested"`
	ID                 string         `json:"id"`
	SessionID          string         `json:"session_id"`
	Sequence           int64          `json:"sequence"`
	Intent             string         `json:"intent"`
	Delivery           State          `json:"delivery"`
	Result             *MessageResult `json:"result"`
	Attention          *Attention     `json:"attention"`
	AdmittedAt         time.Time      `json:"admitted_at"`
}

type MessageResult struct {
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

// Retry acknowledges one caller-keyed request against the Session's existing
// execution authority. Internal task and run identities remain private.
type Retry struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
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

// Sessions keeps domain admission, projection, and cleanup policy outside HTTP.
// Implementations compose Core and return purpose-built public snapshots.
type Sessions interface {
	ReadSandboxStatus(context.Context, string) (provider.Status, error)
	ExecSandbox(context.Context, string, provider.Command) (provider.CommandResult, error)
	List(context.Context, int, string) (SessionList, error)
	Create(context.Context, string, string, CreateSessionRequest) (Session, bool, error)
	Get(context.Context, string) (Session, error)
	SendMessage(context.Context, string, string, SendMessageRequest) (Message, bool, error)
	GetMessage(context.Context, string, string) (Message, error)
	InterruptMessage(context.Context, string, string) (Message, error)
	Retry(context.Context, string, string) (Retry, bool, error)
	ReadSandboxFile(context.Context, string, string) ([]byte, error)
	WriteSandboxFile(context.Context, string, string, []byte, bool) error
	RequestCleanup(context.Context, string) (Session, error)
}
