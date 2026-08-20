package spine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

type CleanupState string

const (
	CleanupPending   CleanupState = "pending"
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
	ActionSandboxCreate     ActionKind = "sandbox-create"
	ActionRepositoryClone   ActionKind = "repository-clone"
	ActionRepositoryRestore ActionKind = "repository-restore"
	ActionRepositorySetup   ActionKind = "repository-setup"
	ActionRepositoryPush    ActionKind = "repository-push"
	ActionGitHubPullRequest ActionKind = "github-pull-request"
	ActionReviewCheckout    ActionKind = "review-checkout"
	ActionRouteCreate       ActionKind = "provider-route-create"
	ActionRouteRevoke       ActionKind = "provider-route-revoke"
	ActionSandboxDelete     ActionKind = "sandbox-delete"
)

type CodebaseInvestigationSourceKind string

const (
	InvestigationSourceRemote    CodebaseInvestigationSourceKind = "remote"
	InvestigationSourceGitBundle CodebaseInvestigationSourceKind = "git-bundle"
)

// CodebaseInvestigationSource is the immutable materialization input for one
// investigation. A retained bundle is input custody, not an output Artifact.
type CodebaseInvestigationSource struct {
	JobID          string                          `json:"job_id,omitempty"`
	Kind           CodebaseInvestigationSourceKind `json:"kind"`
	Repository     string                          `json:"repository,omitempty"`
	Revision       string                          `json:"revision"`
	BundleDigest   string                          `json:"bundle_digest,omitempty"`
	BundleByteSize int64                           `json:"bundle_byte_size,omitempty"`
}

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

const (
	WorkflowCodingToProposal      WorkflowName = "coding-to-proposal"
	WorkflowCodebaseInvestigation WorkflowName = "codebase-investigation"
	CodingToProposalRevision                   = "3"
	CodebaseInvestigationRevision              = "2"
)

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
	Repository              string       `json:"repository"`
	Revision                string       `json:"revision"`
	StartingRevision        string       `json:"starting_revision"`
	Branch                  string       `json:"branch"`
	GitHubRepository        string       `json:"github_repository,omitempty"`
	GitHubInstallation      string       `json:"github_installation_id,omitempty"`
	BaseBranch              string       `json:"base_branch,omitempty"`
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

type Action struct {
	ID        string      `json:"id"`
	JobID     string      `json:"job_id"`
	Kind      ActionKind  `json:"kind"`
	State     ActionState `json:"state"`
	Scope     string      `json:"scope"`
	CreatedAt time.Time   `json:"created_at,omitempty"`
	SettledAt time.Time   `json:"settled_at,omitempty"`
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

type Revision struct {
	JobID          string    `json:"job_id"`
	OID            string    `json:"oid"`
	ComparisonBase string    `json:"comparison_base,omitempty"`
	Tree           string    `json:"tree,omitempty"`
	Branch         string    `json:"branch"`
	Generation     int       `json:"generation"`
	EvidenceID     string    `json:"evidence_id,omitempty"`
	ObservedAt     time.Time `json:"observed_at"`
}

type Check struct {
	ID         string    `json:"id"`
	JobID      string    `json:"job_id"`
	Name       string    `json:"name"`
	Command    string    `json:"command"`
	Revision   string    `json:"revision"`
	State      string    `json:"state"`
	ExitCode   int       `json:"exit_code"`
	EvidenceID string    `json:"evidence_id,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
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
	Kind       string    `json:"kind"`
	ActionID   string    `json:"action_id,omitempty"`
	AgentRunID string    `json:"agent_run_id,omitempty"`
	CheckID    string    `json:"check_id,omitempty"`
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

type GitHubProposal struct {
	JobID            string `json:"job_id"`
	Number           int64  `json:"pr_number"`
	URL              string `json:"pr_url"`
	ProposedRevision string `json:"proposed_revision"`
	BodyDigest       string `json:"body_digest"`
}

type JobOutcomeKind string

const (
	OutcomeAccepted  JobOutcomeKind = "accepted"
	OutcomeRejected  JobOutcomeKind = "rejected"
	OutcomeAbandoned JobOutcomeKind = "abandoned"
)

type JobOutcome struct {
	JobID          string         `json:"job_id"`
	Kind           JobOutcomeKind `json:"outcome"`
	ObservedState  string         `json:"observed_state"`
	ObservedMerged bool           `json:"observed_merged"`
	MergeCommitOID string         `json:"merge_commit_oid,omitempty"`
	ObservedAt     time.Time      `json:"observed_at"`
}

type CodebaseInvestigationDraft struct {
	JobID      string    `json:"job_id"`
	AgentRunID string    `json:"agent_run_id"`
	ArtifactID string    `json:"artifact_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type InvestigationDisposition string

const (
	InvestigationAccepted InvestigationDisposition = "accepted"
	InvestigationRejected InvestigationDisposition = "rejected"
)

// CodebaseInvestigationDecision is the exact human disposition of the latest
// retained draft. It is terminal workflow authority, not agent input.
type CodebaseInvestigationDecision struct {
	JobID       string                   `json:"job_id"`
	ArtifactID  string                   `json:"artifact_id"`
	Disposition InvestigationDisposition `json:"disposition"`
	DecidedBy   string                   `json:"decided_by"`
	Reason      string                   `json:"reason,omitempty"`
	DecidedAt   time.Time                `json:"decided_at"`
}

type HarnessTurn struct {
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	AcceptedMessageIDs []string `json:"accepted_message_ids,omitempty"`
	Output             string   `json:"output,omitempty"`
}

// HarnessBinding is the complete runner-neutral identity of one harness turn.
// ControllerID is transient adapter ownership proof; Dorf does not persist it.
type HarnessBinding struct {
	Harness      string
	ThreadID     string
	Turn         HarnessTurn
	ControllerID string
}

type HarnessHistory struct {
	Harness      string
	ThreadID     string
	Turns        []HarnessTurn
	ControllerID string
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

func CodebaseInvestigationDraftArtifactName(sequence int64) string {
	return fmt.Sprintf("report-%04d.md", sequence)
}

func ReviewRequestFromID(revision, role string) string {
	return "review:" + revision + ":" + role
}

func ReviewRequestMessageID(jobID, revision, role string) string {
	return MessageID(jobID, MessageFromWorkflow, ReviewRequestFromID(revision, role))
}

func ReviewPolicyAttentionSource(revision string) string {
	return "review-policy:" + revision
}

func ReviewSandboxName(runID string) string {
	return "dorf-review-" + digest(runID, 20)
}

// MainSandboxName and ProviderRouteID are exact resource identities derived
// before their external create effects.
func MainSandboxName(jobID string) string {
	return "dorf-" + digest(jobID, 20)
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

func digest(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:length]
}
