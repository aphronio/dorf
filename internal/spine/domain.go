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
	ActionUncertain ActionState = "uncertain"
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
	CleanupState       CleanupState `json:"cleanup_state"`
	TaskID             string       `json:"task_id"`
	CleanupTaskID      string       `json:"cleanup_task_id,omitempty"`
	SandboxID          string       `json:"sandbox_id,omitempty"`
	RouteID            string       `json:"route_id,omitempty"`
	SessionID          string       `json:"session_id,omitempty"`
	AgentRunID         string       `json:"agent_run_id,omitempty"`
	NativeTurnID       string       `json:"native_turn_id,omitempty"`
	NativeOutcome      string       `json:"native_outcome,omitempty"`
	RunTerminalState   string       `json:"run_terminal_state,omitempty"`
}

type Action struct {
	ID         string
	JobID      string
	Kind       ActionKind
	State      ActionState
	ExternalID string
	Outcome    string
}

type Observation struct {
	SessionID  string
	AgentRunID string
	TurnID     string
	Outcome    string
}

type Receipt struct {
	ExternalID string
	Outcome    string
}

func JobID(admissionKey string) string {
	return "job-" + digest(admissionKey, 20)
}

func ActionID(jobID string, kind ActionKind) string {
	return "action-" + digest(jobID+"\x00"+string(kind), 24)
}

func digest(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:length]
}
