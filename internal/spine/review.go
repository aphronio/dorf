package spine

import (
	"context"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
)

const (
	ReviewTriageRole         = "review-triage"
	ReviewReadOnlyCapability = "immutable-read-only"
)

type ReviewActivation struct {
	JobID            string        `json:"job_id"`
	Revision         string        `json:"revision"`
	RequestedRoles   []policy.Role `json:"requested_roles"`
	RequestedByRunID string        `json:"requested_by_agent_run_id,omitempty"`
}

type ReviewPlanRecord struct {
	JobID            string             `json:"job_id"`
	Revision         string             `json:"revision"`
	State            string             `json:"state"`
	Facts            policy.ChangeFacts `json:"facts"`
	Initial          policy.ReviewPlan  `json:"initial_policy"`
	Final            policy.ReviewPlan  `json:"final_plan"`
	PolicyDigest     string             `json:"policy_digest,omitempty"`
	RequestedRoles   []policy.Role      `json:"requested_roles"`
	RequestedByRunID string             `json:"requested_by_agent_run_id,omitempty"`
	TriageRunID      string             `json:"triage_agent_run_id,omitempty"`
	TriageRationale  string             `json:"triage_rationale,omitempty"`
	CreatedAt        time.Time          `json:"created_at,omitempty"`
	FinalizedAt      time.Time          `json:"finalized_at,omitempty"`
}

type ReviewFinding struct {
	RunID          string        `json:"agent_run_id"`
	Revision       string        `json:"revision"`
	Role           policy.Role   `json:"role"`
	Material       bool          `json:"material"`
	Summary        string        `json:"summary"`
	Rationale      string        `json:"rationale"`
	AffectedRoles  []policy.Role `json:"affected_roles"`
	AffectedChecks []string      `json:"affected_checks"`
	EvidenceID     string        `json:"claim_evidence_id,omitempty"`
	Adjudication   string        `json:"adjudication,omitempty"`
	Stale          bool          `json:"stale"`
}

type ReviewRunView struct {
	AgentRun
	Finding *ReviewFinding `json:"finding,omitempty"`
	Stale   bool           `json:"stale"`
}

type ReviewNativeBinding struct {
	AppServerID string
	SessionID   string
	Turn        NativeTurn
}

type ReviewNativeHistory struct {
	AppServerID string
	SessionID   string
	Turns       []NativeTurn
}

const ReviewSubmissionUncertainOutcome = "review-submission-uncertain"

type reviewObservationArtifact struct {
	AgentRunID        string `json:"agent_run_id"`
	Revision          string `json:"revision"`
	Role              string `json:"role"`
	Capability        string `json:"capability"`
	Workspace         string `json:"workspace"`
	SessionID         string `json:"session_id"`
	NativeTurnID      string `json:"native_turn_id"`
	NativeOutcome     string `json:"native_outcome"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CostMicrousd      int64  `json:"cost_microusd"`
	UsageAvailable    bool   `json:"usage_available"`
	ReviewerSandboxID string `json:"reviewer_sandbox_id"`
	ReviewerRouteID   string `json:"reviewer_route_id"`
	ReviewerAppServer string `json:"reviewer_app_server_id"`
	InputDigest       string `json:"input_digest"`
	RevisionTree      string `json:"revision_tree"`
}

type ReviewStore interface {
	MarkChecksVerified(context.Context, string, string, []string) error
	ActivateReview(context.Context, ReviewActivation) (ReviewPlanRecord, bool, error)
	ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error)
	RecordReviewPolicy(context.Context, ReviewPlanRecord) error
	ReviewRuns(context.Context, string, string) ([]ReviewRunView, error)
	AllReviewRuns(context.Context, string) ([]ReviewRunView, error)
	CleanupReviewRuns(context.Context, string) ([]ReviewRunView, error)
	BeginReviewSandbox(context.Context, string) (Action, error)
	BeginReviewRoute(context.Context, string) (Action, error)
	BeginReviewWorkspace(context.Context, string) (Action, error)
	BeginReviewSession(context.Context, string) (Action, error)
	UncertainReviewSubmission(context.Context, string, string, string) error
	ReviewRun(context.Context, string) (AgentRun, error)
	RecordReviewResult(context.Context, string, NativeTurn, Evidence, Evidence, ReviewFinding) error
	RecordTriageResult(context.Context, string, NativeTurn, Evidence, Evidence, policy.ReviewPlan, string) error
	AdmitReviewRepair(context.Context, string, string) (Message, bool, error)
	MarkReviewReady(context.Context, string, string) error
	BeginReviewRouteCleanup(context.Context, string) (Action, error)
	BeginReviewSandboxCleanup(context.Context, string) (Action, error)
	InterruptReviewRun(context.Context, string, string) error
	RecordReviewPostState(context.Context, string, Receipt) error
	ReviewRepairTargets(context.Context, string) ([]policy.Role, error)
	RejectReviewFinding(context.Context, string, string) error
}

type ReviewExternals interface {
	RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error)
	ReviewSandboxCreate(context.Context, Job, AgentRun, Action) (Receipt, error)
	ReviewRouteCreate(context.Context, Job, AgentRun, Action) (Receipt, error)
	ReviewWorkspaceCreate(context.Context, Job, AgentRun, Action) (Receipt, error)
	ReviewWorkspaceVerify(context.Context, Job, AgentRun) (Receipt, error)
	ReviewRouteRevoke(context.Context, Job, AgentRun, Action) (Receipt, error)
	ReviewSandboxDelete(context.Context, Job, AgentRun, Action) (Receipt, error)
	ReviewInitialTurn(context.Context, Job, AgentRun) (ReviewNativeBinding, error)
	ReviewRecover(context.Context, Job, AgentRun) (ReviewNativeBinding, error)
	ReviewTurns(context.Context, Job, AgentRun) (ReviewNativeHistory, error)
	ReviewWait(context.Context, Job, AgentRun, string) (ReviewNativeBinding, error)
}

type ReviewAdjudicationExternals interface {
	RepositoryHasChanges(context.Context, Job) (bool, error)
}
