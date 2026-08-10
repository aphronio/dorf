package spine

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type CleanupState string

const (
	CleanupPending   CleanupState = "pending"
	CleanupScheduled CleanupState = "scheduled"
	CleanupComplete  CleanupState = "complete"
)

type ActionKind string

const (
	ActionSandboxCreate         ActionKind = "sandbox-create"
	ActionRepositoryClone       ActionKind = "repository-clone"
	ActionRepositorySetup       ActionKind = "repository-setup"
	ActionRepositoryPush        ActionKind = "repository-push"
	ActionGitHubPullRequest     ActionKind = "github-pull-request"
	ActionReviewWorkspaceCreate ActionKind = "review-workspace-create"
	ActionRouteCreate           ActionKind = "provider-route-create"
	ActionSessionStart          ActionKind = "codex-session-start"
	ActionTurnStart             ActionKind = "codex-turn-start"
	ActionRouteRevoke           ActionKind = "provider-route-revoke"
	ActionSandboxDelete         ActionKind = "sandbox-delete"
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
	ID                   string       `json:"id"`
	AdmissionKey         string       `json:"admission_key"`
	Goal                 string       `json:"goal"`
	Repository           string       `json:"repository"`
	Revision             string       `json:"revision"`
	RevisionGeneration   int          `json:"revision_generation"`
	StartingRevision     string       `json:"starting_revision"`
	Branch               string       `json:"branch"`
	GitHubRepository     string       `json:"github_repository,omitempty"`
	GitHubInstallation   string       `json:"github_installation_id,omitempty"`
	BaseBranch           string       `json:"base_branch,omitempty"`
	ProviderConnection   string       `json:"provider_connection"`
	ProviderGatewayState string       `json:"provider_gateway_state,omitempty"`
	Model                string       `json:"model"`
	ReasoningEffort      string       `json:"reasoning_effort"`
	AdmissionOpen        bool         `json:"admission_open"`
	CleanupState         CleanupState `json:"cleanup_state"`
	TaskID               string       `json:"task_id"`
	CleanupTaskID        string       `json:"cleanup_task_id,omitempty"`
	SandboxID            string       `json:"sandbox_id,omitempty"`
	SandboxState         string       `json:"sandbox_state,omitempty"`
	RouteID              string       `json:"route_id,omitempty"`
	RouteState           string       `json:"route_state,omitempty"`
	SessionID            string       `json:"session_id,omitempty"`
	WorkflowPhase        string       `json:"workflow_phase"`
	WorkflowAttention    string       `json:"workflow_attention,omitempty"`
	CleanupAttention     string       `json:"cleanup_attention,omitempty"`
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
}

type MessageDeliveryIntent string

const (
	MessageFollow MessageDeliveryIntent = "follow"
	MessageSteer  MessageDeliveryIntent = "steer"
)

type AgentRun struct {
	ID                string        `json:"id"`
	JobID             string        `json:"job_id"`
	MessageID         string        `json:"message_id"`
	ActionID          string        `json:"action_id"`
	SessionID         string        `json:"session_id"`
	State             AgentRunState `json:"state"`
	BaselineRecorded  bool          `json:"baseline_recorded"`
	BaselineTurnID    string        `json:"baseline_turn_id,omitempty"`
	NativeTurnID      string        `json:"native_turn_id,omitempty"`
	NativeOutcome     string        `json:"native_outcome,omitempty"`
	Attention         string        `json:"attention,omitempty"`
	Role              string        `json:"role"`
	Revision          string        `json:"revision,omitempty"`
	Capability        string        `json:"capability,omitempty"`
	Workspace         string        `json:"workspace,omitempty"`
	InputContract     string        `json:"input_contract,omitempty"`
	StartedAt         time.Time     `json:"started_at,omitempty"`
	FinishedAt        time.Time     `json:"finished_at,omitempty"`
	InputTokens       int64         `json:"input_tokens,omitempty"`
	CachedInputTokens int64         `json:"cached_input_tokens,omitempty"`
	OutputTokens      int64         `json:"output_tokens,omitempty"`
	CostMicrousd      int64         `json:"cost_microusd,omitempty"`
	UsageAvailable    bool          `json:"usage_available"`
	YieldCount        int           `json:"yield_count,omitempty"`
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
	Delivered      bool          `json:"delivered"`
}

type Action struct {
	ID         string
	JobID      string
	MessageID  string
	Kind       ActionKind
	State      ActionState
	ExternalID string
	Outcome    string
	Scope      string
}

type CommandObservation struct {
	Command    string
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	Stdout     []byte
	Stderr     []byte
	StdoutCut  bool
	StderrCut  bool
	Redactions []string
}

type RevisionObservation struct {
	ComparisonBase string    `json:"comparison_base"`
	Revision       string    `json:"revision"`
	Tree           string    `json:"tree"`
	Branch         string    `json:"branch"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

type Check struct {
	ID             string    `json:"id"`
	JobID          string    `json:"job_id"`
	Name           string    `json:"name"`
	Command        string    `json:"command"`
	Revision       string    `json:"revision"`
	State          string    `json:"state"`
	ExitCode       int       `json:"exit_code"`
	EvidenceID     string    `json:"evidence_id,omitempty"`
	EvidenceDigest string    `json:"evidence_digest,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	FinishedAt     time.Time `json:"finished_at,omitempty"`
}

type DeclaredCheck struct {
	Name    string
	Command string
}

type Evidence struct {
	ID         string    `json:"id"`
	Digest     string    `json:"digest"`
	ByteSize   int64     `json:"byte_size"`
	MediaType  string    `json:"media_type"`
	Producer   string    `json:"producer"`
	Provenance string    `json:"provenance"`
	Kind       string    `json:"kind"`
	ActionID   string    `json:"action_id,omitempty"`
	CheckID    string    `json:"check_id,omitempty"`
	Revision   string    `json:"revision,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

type Receipt struct {
	ExternalID string
	Outcome    string
}

type GitHubProposal struct {
	JobID              string `json:"job_id"`
	Repository         string `json:"repository"`
	InstallationID     string `json:"installation_id"`
	BaseBranch         string `json:"base_branch"`
	HeadBranch         string `json:"head_branch"`
	Number             int64  `json:"pr_number"`
	URL                string `json:"pr_url"`
	ProposedRevision   string `json:"proposed_revision"`
	ObservedRemoteHead string `json:"observed_remote_head"`
	BodyDigest         string `json:"body_digest"`
	Stale              bool   `json:"stale"`
}

type JobOutcomeKind string

const (
	OutcomeAccepted  JobOutcomeKind = "accepted"
	OutcomeRejected  JobOutcomeKind = "rejected"
	OutcomeAbandoned JobOutcomeKind = "abandoned"
)

type JobOutcome struct {
	JobID            string         `json:"job_id"`
	Kind             JobOutcomeKind `json:"outcome"`
	Repository       string         `json:"repository"`
	InstallationID   string         `json:"installation_id"`
	BaseBranch       string         `json:"base_branch"`
	HeadBranch       string         `json:"head_branch"`
	Number           int64          `json:"pr_number"`
	URL              string         `json:"pr_url"`
	ProposedRevision string         `json:"proposed_revision"`
	ObservedHead     string         `json:"observed_head"`
	ObservedState    string         `json:"observed_state"`
	ObservedMerged   bool           `json:"observed_merged"`
	MergeCommitOID   string         `json:"merge_commit_oid,omitempty"`
	ObservedAt       time.Time      `json:"observed_at"`
}

type NativeTurn struct {
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	AcceptedMessageIDs []string `json:"accepted_message_ids,omitempty"`
	Output             string   `json:"output,omitempty"`
	InputTokens        int64    `json:"input_tokens,omitempty"`
	CachedInputTokens  int64    `json:"cached_input_tokens,omitempty"`
	OutputTokens       int64    `json:"output_tokens,omitempty"`
	CostMicrousd       int64    `json:"cost_microusd,omitempty"`
	UsageAvailable     bool     `json:"usage_available"`
}

type Reconciliation struct {
	Classification string
	Turn           NativeTurn
	Reason         string
}

func JobID(admissionKey string) string {
	return "job-" + digest(admissionKey, 20)
}

func MessageID(jobID string, fromKind MessageFromKind, fromID string) string {
	return "message-" + digest(jobID+"\x00"+string(fromKind)+"\x00"+fromID, 24)
}

func AgentRunID(messageID string) string {
	return "agent-run-" + digest(messageID+"\x00implement", 24)
}

func ReviewAgentRunID(jobID, revision, role string) string {
	return "agent-run-" + digest(jobID+"\x00"+revision+"\x00"+role, 24)
}

func ReviewSandboxName(runID string) string {
	return "dorf-review-" + digest(runID, 20)
}

// MainSandboxName and ProviderRouteID are the exact resource identities
// durably reserved before their external create effects.
func MainSandboxName(jobID string) string {
	return "dorf-" + digest(jobID, 20)
}

func ProviderRouteID(actionID string) string {
	return "route-" + digest(actionID, 16)
}

func ReviewControllerID(runID, sandboxName, ownershipNonce string) string {
	return "review-controller-" + digest(runID+"\x00"+sandboxName+"\x00"+ownershipNonce, 32)
}

func ActionID(jobID string, kind ActionKind) string {
	return "action-" + digest(jobID+"\x00"+string(kind), 24)
}

func ScopedActionID(jobID string, kind ActionKind, scope string) string {
	return "action-" + digest(jobID+"\x00"+string(kind)+"\x00"+scope, 24)
}

func CheckID(jobID, revision, name string) string {
	return "check-" + digest(jobID+"\x00"+revision+"\x00"+name, 24)
}

func EvidenceID(ownerID, kind string) string {
	return "evidence-" + digest(ownerID+"\x00"+kind, 24)
}

func TurnActionID(messageID string) string {
	return "action-" + digest(messageID+"\x00"+string(ActionTurnStart), 24)
}

func digest(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:length]
}
