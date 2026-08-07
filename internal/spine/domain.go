package spine

import (
	"crypto/sha256"
	"encoding/hex"
)

type JobState string

const (
	JobAdmitted JobState = "admitted"
	JobRunning  JobState = "running"
	JobObserved JobState = "observed"
	JobFailed   JobState = "failed"
)

type CleanupState string

const (
	CleanupPending   CleanupState = "pending"
	CleanupScheduled CleanupState = "scheduled"
	CleanupComplete  CleanupState = "complete"
)

type ActionKind string

const (
	ActionSandboxCreate   ActionKind = "sandbox-create"
	ActionRepositoryClone ActionKind = "repository-clone"
	ActionRouteCreate     ActionKind = "provider-route-create"
	ActionSessionStart    ActionKind = "codex-session-start"
	ActionTurnStart       ActionKind = "codex-turn-start"
	ActionRouteRevoke     ActionKind = "provider-route-revoke"
	ActionSandboxDelete   ActionKind = "sandbox-delete"
)

type ActionState string

const (
	ActionPending   ActionState = "pending"
	ActionSucceeded ActionState = "succeeded"
	ActionFailed    ActionState = "failed"
	ActionUncertain ActionState = "uncertain"
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

type Job struct {
	ID                 string       `json:"id"`
	AdmissionKey       string       `json:"admission_key"`
	Goal               string       `json:"goal"`
	Repository         string       `json:"repository"`
	Revision           string       `json:"revision"`
	Branch             string       `json:"branch"`
	ProviderConnection string       `json:"provider_connection"`
	Model              string       `json:"model"`
	ReasoningEffort    string       `json:"reasoning_effort"`
	State              JobState     `json:"state"`
	AdmissionOpen      bool         `json:"admission_open"`
	CleanupState       CleanupState `json:"cleanup_state"`
	TaskID             string       `json:"task_id"`
	CleanupTaskID      string       `json:"cleanup_task_id,omitempty"`
	SandboxID          string       `json:"sandbox_id,omitempty"`
	RouteID            string       `json:"route_id,omitempty"`
	SessionID          string       `json:"session_id,omitempty"`
	RunTerminalState   string       `json:"run_terminal_state,omitempty"`
}

type Message struct {
	ID       string `json:"id"`
	JobID    string `json:"job_id"`
	CallerID string `json:"caller_id"`
	Sequence int64  `json:"sequence"`
	Input    string `json:"input"`
}

type AgentRun struct {
	ID               string        `json:"id"`
	JobID            string        `json:"job_id"`
	MessageID        string        `json:"message_id"`
	ActionID         string        `json:"action_id"`
	SessionID        string        `json:"session_id"`
	State            AgentRunState `json:"state"`
	BaselineRecorded bool          `json:"baseline_recorded"`
	BaselineTurnID   string        `json:"baseline_turn_id,omitempty"`
	NativeTurnID     string        `json:"native_turn_id,omitempty"`
	NativeOutcome    string        `json:"native_outcome,omitempty"`
	Attention        string        `json:"attention,omitempty"`
}

type Delivery struct {
	Message  Message  `json:"message"`
	AgentRun AgentRun `json:"agent_run"`
}

type MessageView struct {
	Message
	AgentRunID     string        `json:"agent_run_id,omitempty"`
	State          AgentRunState `json:"state,omitempty"`
	NativeTurnID   string        `json:"native_turn_id,omitempty"`
	NativeOutcome  string        `json:"native_outcome,omitempty"`
	Attention      string        `json:"attention,omitempty"`
	BlockingSeq    int64         `json:"blocking_sequence,omitempty"`
	BlockingReason string        `json:"blocking_reason,omitempty"`
}

type Action struct {
	ID         string
	JobID      string
	MessageID  string
	Kind       ActionKind
	State      ActionState
	ExternalID string
	Outcome    string
}

type Receipt struct {
	ExternalID string
	Outcome    string
}

type NativeTurn struct {
	ID     string
	Status string
}

type Reconciliation struct {
	Classification string
	Turn           NativeTurn
	Reason         string
}

func JobID(admissionKey string) string {
	return "job-" + digest(admissionKey, 20)
}

func MessageID(jobID, callerID string) string {
	return "message-" + digest(jobID+"\x00"+callerID, 24)
}

func AgentRunID(messageID string) string {
	return "agent-run-" + digest(messageID+"\x00implement", 24)
}

func ActionID(jobID string, kind ActionKind) string {
	return "action-" + digest(jobID+"\x00"+string(kind), 24)
}

func TurnActionID(messageID string) string {
	return "action-" + digest(messageID+"\x00"+string(ActionTurnStart), 24)
}

func digest(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:length]
}
