package spine

import (
	"context"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
)

const (
	ReviewReadOnlyCapability = "immutable-read-only"
)

type ReviewPlanRecord struct {
	JobID        string             `json:"job_id"`
	Revision     string             `json:"revision"`
	State        string             `json:"state"`
	Facts        policy.ChangeFacts `json:"facts"`
	Plan         policy.ReviewPlan  `json:"plan"`
	PolicyDigest string             `json:"policy_digest,omitempty"`
	CreatedAt    time.Time          `json:"created_at,omitempty"`
	FinalizedAt  time.Time          `json:"finalized_at,omitempty"`
}

type ReviewRunView struct {
	AgentRun
	Request Message `json:"request"`
	ReviewRunProjection
	FeedbackMessageID string `json:"feedback_message_id,omitempty"`
	Stale             bool   `json:"stale"`
}

// ReviewRunProjection contains review workflow facts derived from an AgentRun
// and its isolated review resources. They are intentionally not part of the
// generic AgentRun contract.
type ReviewRunProjection struct {
	ReviewerSandboxID    string `json:"reviewer_sandbox_id,omitempty"`
	ReviewerRouteID      string `json:"reviewer_route_id,omitempty"`
	ReviewerOwnerNonce   string `json:"-"`
	SubmissionNonce      string `json:"-"`
	RevisionTree         string `json:"revision_tree,omitempty"`
	ReviewerSandboxState string `json:"reviewer_sandbox_state,omitempty"`
	ReviewerRouteState   string `json:"reviewer_route_state,omitempty"`
	CheckoutState        string `json:"checkout_state,omitempty"`
	PostReviewState      string `json:"post_review_state,omitempty"`
}

type reviewObservationArtifact struct {
	AgentRunID  string `json:"agent_run_id"`
	Revision    string `json:"revision"`
	Role        string `json:"role"`
	Capability  string `json:"capability"`
	Harness     string `json:"harness"`
	ThreadID    string `json:"thread_id"`
	TurnID      string `json:"turn_id"`
	TurnOutcome string `json:"turn_outcome"`
}

type ReviewStore interface {
	MarkChecksVerified(context.Context, string, string, []string) error
	ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error)
	RecordReviewPolicy(context.Context, ReviewPlanRecord) error
	ReviewRuns(context.Context, string, string) ([]ReviewRunView, error)
	AllReviewRuns(context.Context, string) ([]ReviewRunView, error)
	CleanupReviewRuns(context.Context, string) ([]ReviewRunView, error)
	BeginReviewSandbox(context.Context, string) (Action, error)
	BeginReviewRoute(context.Context, string) (Action, error)
	BeginReviewWorkspace(context.Context, string) (Action, error)
	ReviewRun(context.Context, string) (ReviewRunView, error)
	RecordReviewFeedback(context.Context, string, HarnessTurn, Evidence) (Message, bool, error)
	CompleteReviewFeedback(context.Context, string, string, string) (bool, error)
	BeginReviewRouteCleanup(context.Context, string) (Action, error)
	BeginReviewSandboxCleanup(context.Context, string) (Action, error)
	InterruptReviewRun(context.Context, string, string) error
	RecordReviewPostState(context.Context, string, Receipt) error
}

type ReviewExternals interface {
	RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error)
	ReviewSandboxCreate(context.Context, Job, ReviewRunView, Action) (Receipt, error)
	ReviewRouteCreate(context.Context, Job, ReviewRunView, Action) (Receipt, error)
	ReviewWorkspaceCreate(context.Context, Job, ReviewRunView, Action) (Receipt, error)
	ReviewWorkspaceVerify(context.Context, Job, ReviewRunView) (Receipt, error)
	ReviewRouteRevoke(context.Context, Job, ReviewRunView, Action) (Receipt, error)
	ReviewSandboxDelete(context.Context, Job, ReviewRunView, Action) (Receipt, error)
	ReviewInitialTurn(context.Context, Job, ReviewRunView) (HarnessBinding, error)
	ReviewRecover(context.Context, Job, ReviewRunView) (HarnessBinding, error)
	ReviewTurns(context.Context, Job, ReviewRunView) (HarnessHistory, error)
	ReviewWait(context.Context, Job, ReviewRunView, string) (HarnessBinding, error)
}
