// Package core owns Dorf's in-process control-plane contract, durable custody
// records, and recovery services. Workflows consume this boundary without
// inheriting provider, harness, or persistence implementations.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type CleanupState string

const (
	CleanupPending   CleanupState = "pending"
	CleanupRequested CleanupState = "requested"
	CleanupScheduled CleanupState = "scheduled"
	CleanupComplete  CleanupState = "complete"
)

// JobTask is one immutable attachment from a Dorf Job to an Absurd task.
// Sequence expresses handoff order; Absurd remains authoritative for task
// execution, attempts, checkpoints, and terminal state.
type JobTask struct {
	JobID      string    `json:"job_id"`
	Sequence   int64     `json:"sequence"`
	TaskID     string    `json:"task_id"`
	TaskName   string    `json:"task_name"`
	AttachedAt time.Time `json:"attached_at"`
}

type ActionKind string

const (
	ActionSandboxCreate ActionKind = "sandbox-create"
	ActionRouteCreate   ActionKind = "provider-route-create"
	ActionRouteRevoke   ActionKind = "provider-route-revoke"
	ActionSandboxDelete ActionKind = "sandbox-delete"
)

type ActionState string

const (
	ActionUnsettled ActionState = "unsettled"
	ActionSucceeded ActionState = "succeeded"
	ActionFailed    ActionState = "failed"
)

type AgentRunState string

const (
	AgentRunPending     AgentRunState = "pending"
	AgentRunSubmitting  AgentRunState = "submitting"
	AgentRunActive      AgentRunState = "active"
	AgentRunCompleted   AgentRunState = "completed"
	AgentRunFailed      AgentRunState = "failed"
	AgentRunInterrupted AgentRunState = "interrupted"
	AgentRunUncertain   AgentRunState = "uncertain"
)

type WorkflowName string

type SandboxProvider string

const (
	SandboxProviderIncus SandboxProvider = "incus"
	SandboxProviderE2B   SandboxProvider = "e2b"
	BaseProfileContract                  = "base-1"
)

// SandboxProfile is one named provider, artifact, and Harness definition
// selected by name at Job admission. It is immutable while referenced by an
// incompletely cleaned Job. Provider credentials remain deployment secrets.
type SandboxProfile struct {
	Name              string               `json:"name"`
	Provider          SandboxProvider      `json:"provider"`
	Harness           string               `json:"harness"`
	Artifact          string               `json:"artifact"`
	IncusNetwork      string               `json:"incus_network,omitempty"`
	IncusDiskSize     string               `json:"incus_disk_size,omitempty"`
	E2BGatewayURL     string               `json:"e2b_gateway_url,omitempty"`
	E2BSandboxTimeout time.Duration        `json:"e2b_sandbox_timeout,omitempty"`
	E2BAllowInternet  bool                 `json:"e2b_allow_internet,omitempty"`
	Default           bool                 `json:"default"`
	CreatedAt         time.Time            `json:"created_at,omitempty"`
	Verification      *ProfileVerification `json:"verification,omitempty"`
}

type ProfileVerification struct {
	ProfileName      string    `json:"profile_name"`
	ContractVersion  string    `json:"contract_version"`
	SandboxID        string    `json:"sandbox_id"`
	OwnershipNonce   string    `json:"-"`
	HarnessVersion   string    `json:"harness_version,omitempty"`
	AttemptedAt      time.Time `json:"attempted_at,omitempty"`
	ProbeCompletedAt time.Time `json:"probe_completed_at,omitempty"`
	CleanedAt        time.Time `json:"cleaned_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
}

func (p SandboxProfile) BaseVerified() bool {
	return p.Verification != nil && p.Verification.ContractVersion == BaseProfileContract &&
		!p.Verification.ProbeCompletedAt.IsZero() && !p.Verification.CleanedAt.IsZero() &&
		p.Verification.LastError == ""
}

type Job struct {
	ID                      string       `json:"id"`
	AdmissionKey            string       `json:"admission_key"`
	Workflow                WorkflowName `json:"workflow"`
	WorkflowRevision        string       `json:"workflow_revision"`
	Goal                    string       `json:"goal"`
	SandboxProfile          string       `json:"sandbox_profile"`
	ProviderConnection      string       `json:"provider_connection"`
	Model                   string       `json:"model"`
	ReasoningEffort         string       `json:"reasoning_effort"`
	AdmissionOpen           bool         `json:"admission_open"`
	CleanupState            CleanupState `json:"cleanup_state"`
	CurrentTaskID           string       `json:"current_task_id,omitempty"`
	WorkflowAttention       string       `json:"workflow_attention,omitempty"`
	WorkflowAttentionSource string       `json:"workflow_attention_source,omitempty"`
	WorkflowAttentionAt     time.Time    `json:"workflow_attention_at,omitempty"`
	CleanupAttention        string       `json:"cleanup_attention,omitempty"`
	AdmittedAt              time.Time    `json:"admitted_at,omitempty"`
	CleanedAt               time.Time    `json:"cleaned_at,omitempty"`
}

// Sandbox is infrastructure owned for the lifetime of a Job. AgentRuns use a
// Sandbox, but never own it.
type Sandbox struct {
	ID             string `json:"id"`
	JobID          string `json:"job_id"`
	Name           string `json:"name"`
	OwnershipNonce string `json:"-"`
}

// Route is the deterministic provider route serving one Sandbox. Its
// lifecycle is recorded by the Sandbox's route Actions.
type Route struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandbox_id"`
}

type MessageFromKind string

const (
	MessageFromHuman    MessageFromKind = "human"
	MessageFromAgent    MessageFromKind = "agent"
	MessageFromWorkflow MessageFromKind = "workflow"
)

type Message struct {
	ID           string                `json:"id"`
	JobID        string                `json:"job_id"`
	FromKind     MessageFromKind       `json:"from_kind"`
	FromID       string                `json:"from_id"`
	Sequence     int64                 `json:"sequence"`
	Input        string                `json:"input"`
	Intent       MessageDeliveryIntent `json:"intent"`
	TargetTurnID string                `json:"target_turn_id,omitempty"`
	AdmittedAt   time.Time             `json:"admitted_at,omitempty"`
}

type MessageDeliveryIntent string

const (
	MessageFollow MessageDeliveryIntent = "follow"
	MessageSteer  MessageDeliveryIntent = "steer"
)

// AgentRun is the durable delivery of one Message to an agent harness. A follow
// normally binds a new Turn; a steer normally binds its target Turn and creates
// a new Turn only when terminal-target fallback is required.
type AgentRun struct {
	ID               string        `json:"id"`
	JobID            string        `json:"job_id"`
	MessageID        string        `json:"message_id"`
	Harness          string        `json:"harness,omitempty"`
	ThreadID         string        `json:"thread_id,omitempty"`
	State            AgentRunState `json:"state"`
	BaselineRecorded bool          `json:"baseline_recorded"`
	BaselineTurnID   string        `json:"baseline_turn_id,omitempty"`
	TurnID           string        `json:"turn_id,omitempty"`
	TurnOutcome      string        `json:"turn_outcome,omitempty"`
	Attention        string        `json:"attention,omitempty"`
	Role             string        `json:"role"`
	// InputRevision is the accepted checkout when this delivery begins. A
	// later git-revision Evidence records what the AgentRun left behind.
	InputRevision   string    `json:"input_revision,omitempty"`
	Capability      string    `json:"capability,omitempty"`
	SandboxID       string    `json:"sandbox_id,omitempty"`
	SubmissionNonce string    `json:"-"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	FinishedAt      time.Time `json:"finished_at,omitempty"`
}

type Delivery struct {
	Message  Message  `json:"message"`
	AgentRun AgentRun `json:"agent_run"`
}

// AgentMessageExecution is Core's authoritative private execution aggregate.
// Consumers address work by Message identity; Core reloads the internal
// AgentRun and exact Job-owned Sandbox before touching the Harness.
type AgentMessageExecution struct {
	Job      Job
	Message  Message
	AgentRun AgentRun
	Sandbox  Sandbox
}

// MessageResult is the smallest consumer observation of one admitted Message.
// An empty Outcome means that the Harness work has not reached a terminal
// result yet. Harness Thread, Turn, and AgentRun identity remain internal.
type MessageResult struct {
	MessageID string `json:"message_id"`
	Outcome   string `json:"outcome,omitempty"`
	Output    string `json:"output,omitempty"`
}

func (r MessageResult) Terminal() bool { return r.Outcome != "" }

// AgentMessageWork is the opaque static-composition result that one exact
// Message in one exact Sandbox still needs Core reconciliation. Core consumes
// it inside the Job fence; workflow coordinators never receive it.
type AgentMessageWork struct {
	MessageID string `json:"message_id"`
	SandboxID string `json:"sandbox_id"`
}

type Action struct {
	ID        string      `json:"id"`
	JobID     string      `json:"job_id"`
	Kind      ActionKind  `json:"kind"`
	State     ActionState `json:"state"`
	Scope     string      `json:"scope"`
	CreatedAt time.Time   `json:"created_at,omitempty"`
	SettledAt time.Time   `json:"settled_at,omitempty"`
}

// SandboxActionAuthorization is the authoritative persisted provider-effect
// tuple, including the exact current Absurd task attachment.
type SandboxActionAuthorization struct {
	Job      Job
	Sandbox  Sandbox
	Action   Action
	TaskID   string
	TaskName string
}

type Evidence struct {
	ID         string    `json:"id"`
	Digest     string    `json:"digest"`
	ByteSize   int64     `json:"byte_size"`
	MediaType  string    `json:"media_type"`
	Producer   string    `json:"producer"`
	Kind       string    `json:"kind"`
	ActionID   string    `json:"action_id,omitempty"`
	AgentRunID string    `json:"agent_run_id,omitempty"`
	Revision   string    `json:"revision,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Artifact is one immutable, named deliverable produced by a workflow. Its
// bytes live in the deployment-owned content-addressed store; this record is
// the durable Job-scoped identity used for discovery and retrieval.
type Artifact struct {
	ID         string    `json:"id"`
	JobID      string    `json:"job_id"`
	Name       string    `json:"name"`
	Digest     string    `json:"digest"`
	ByteSize   int64     `json:"byte_size"`
	MediaType  string    `json:"media_type"`
	Producer   string    `json:"producer"`
	AgentRunID string    `json:"agent_run_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type HarnessTurn struct {
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	AcceptedMessageIDs []string `json:"accepted_message_ids,omitempty"`
	Output             string   `json:"output,omitempty"`
}

// Terminal reports whether the Harness has settled this Turn.
func (t HarnessTurn) Terminal() bool {
	return t.Status == "completed" || t.Status == "failed" || t.Status == "interrupted"
}

// HarnessBinding is the complete runner-neutral identity of one harness turn.
type HarnessBinding struct {
	Harness  string
	ThreadID string
	Turn     HarnessTurn
}

type HarnessHistory struct {
	Harness  string
	ThreadID string
	Turns    []HarnessTurn
}

type Reconciliation struct {
	Classification string
	Turn           HarnessTurn
	Reason         string
}

func JobID(admissionKey string) string {
	return "job-" + digest(admissionKey, 20)
}

func MessageID(jobID string, fromKind MessageFromKind, fromID string) string {
	return "message-" + digest(jobID+"\x00"+string(fromKind)+"\x00"+fromID, 24)
}

func AgentRunID(messageID string) string {
	return "agent-run-" + digest(messageID, 24)
}

func ArtifactID(jobID, name string) string {
	return "artifact-" + digest(jobID+"\x00"+name, 24)
}

const DefaultSandbox = "default"

// MainSandboxName and ProviderRouteID are exact resource identities derived
// before their external create effects. MainSandboxName remains the stable
// identity of the Job's default Sandbox.
func MainSandboxName(jobID string) string {
	return "dorf-" + digest(jobID, 20)
}

func NamedSandboxID(jobID, name string) string {
	if name == DefaultSandbox {
		return MainSandboxName(jobID)
	}
	return "dorf-" + digest(jobID+"\x00sandbox\x00"+name, 20)
}

func ProviderRouteID(actionID string) string {
	return "route-" + digest(actionID, 16)
}

func RouteForSandbox(sandbox Sandbox) Route {
	return Route{
		ID:        ProviderRouteID(ScopedActionID(sandbox.JobID, ActionRouteCreate, sandbox.ID)),
		SandboxID: sandbox.ID,
	}
}

func ActionID(jobID string, kind ActionKind) string {
	return "action-" + digest(jobID+"\x00"+string(kind), 24)
}

func ScopedActionID(jobID string, kind ActionKind, scope string) string {
	return "action-" + digest(jobID+"\x00"+string(kind)+"\x00"+scope, 24)
}

func EvidenceID(ownerID, kind string) string {
	return "evidence-" + digest(ownerID+"\x00"+kind, 24)
}

func digest(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:length]
}
