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
	Request           Message `json:"request"`
	Sandbox           Sandbox `json:"sandbox"`
	Route             Route   `json:"route"`
	FeedbackMessageID string  `json:"feedback_message_id,omitempty"`
	Stale             bool    `json:"stale"`
}

type reviewObservationArtifact struct {
	AgentRunID  string                     `json:"agent_run_id"`
	Revision    string                     `json:"revision"`
	Role        string                     `json:"role"`
	Capability  string                     `json:"capability"`
	Harness     string                     `json:"harness"`
	ThreadID    string                     `json:"thread_id"`
	TurnID      string                     `json:"turn_id"`
	TurnOutcome string                     `json:"turn_outcome"`
	PostState   ReviewWorkspaceObservation `json:"post_state"`
}

type ReviewWorkspaceObservation struct {
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}

type ReviewStore interface {
	MarkChecksVerified(context.Context, string, string, []string) error
	ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error)
	RecordReviewPolicy(context.Context, ReviewPlanRecord) error
	ReviewRuns(context.Context, string, string) ([]ReviewRunView, error)
	AllReviewRuns(context.Context, string) ([]ReviewRunView, error)
	BeginReviewWorkspace(context.Context, string) (Action, error)
	ReviewRun(context.Context, string) (ReviewRunView, error)
	RecordReviewFeedback(context.Context, string, HarnessTurn, Evidence) (Message, bool, error)
	CompleteReviewFeedback(context.Context, string, string, string) (bool, error)
}

type ReviewExternals interface {
	RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error)
	ReviewWorkspaceCreate(context.Context, Job, ReviewRunView, Action) (Receipt, error)
	ReviewWorkspaceVerify(context.Context, Job, ReviewRunView) (ReviewWorkspaceObservation, error)
	ReviewInitialTurn(context.Context, Job, ReviewRunView) (HarnessBinding, error)
	ReviewRecover(context.Context, Job, ReviewRunView) (HarnessBinding, error)
	ReviewTurns(context.Context, Job, ReviewRunView) (HarnessHistory, error)
	ReviewWait(context.Context, Job, ReviewRunView, string) (HarnessBinding, error)
}
