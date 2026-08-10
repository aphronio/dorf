package spine

import (
	"context"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
)

const (
	ReviewReadOnlyCapability = "immutable-read-only"
)

// ReviewPlanRecord exists only after deterministic policy has made its final
// decision for an exact Revision. There is no pending plan state.
type ReviewPlanRecord struct {
	JobID      string             `json:"job_id"`
	Revision   string             `json:"revision"`
	Facts      policy.ChangeFacts `json:"facts"`
	Plan       policy.ReviewPlan  `json:"plan"`
	RecordedAt time.Time          `json:"recorded_at,omitempty"`
}

type ReviewRunView struct {
	AgentRun
	Request Message `json:"request"`
	Sandbox Sandbox `json:"sandbox"`
}

type reviewObservationArtifact struct {
	AgentRunID  string                    `json:"agent_run_id"`
	Revision    string                    `json:"revision"`
	Role        string                    `json:"role"`
	Capability  string                    `json:"capability"`
	Harness     string                    `json:"harness"`
	ThreadID    string                    `json:"thread_id"`
	TurnID      string                    `json:"turn_id"`
	TurnOutcome string                    `json:"turn_outcome"`
	Checkout    ReviewCheckoutObservation `json:"checkout"`
}

type ReviewCheckoutObservation struct {
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}

type ReviewStore interface {
	ReviewPlan(context.Context, string, string) (ReviewPlanRecord, error)
	RecordReviewPolicy(context.Context, ReviewPlanRecord) error
	ReviewRuns(context.Context, string, string) ([]ReviewRunView, error)
	AllReviewRuns(context.Context, string) ([]ReviewRunView, error)
	ReviewRun(context.Context, string) (ReviewRunView, error)
	RecordReviewFeedback(context.Context, string, HarnessTurn, Evidence) (Message, bool, error)
}

type ReviewExternals interface {
	RepositoryChangeFacts(context.Context, Job) (policy.ChangeFacts, error)
	PrepareReviewCheckout(context.Context, Job, ReviewRunView) error
	VerifyReviewCheckout(context.Context, Job, ReviewRunView) (ReviewCheckoutObservation, error)
	ReviewInitialTurn(context.Context, Job, ReviewRunView) (HarnessBinding, error)
	ReviewRecover(context.Context, Job, ReviewRunView) (HarnessBinding, error)
	ReviewTurns(context.Context, Job, ReviewRunView) (HarnessHistory, error)
	ReviewWait(context.Context, Job, ReviewRunView, string) (HarnessBinding, error)
}
