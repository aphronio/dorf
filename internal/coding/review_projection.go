package coding

import (
	"time"

	"github.com/aphronio/dorf/internal/core"
	policy "github.com/aphronio/dorf/internal/review"
)

const ReviewReadOnlyCapability = "immutable-read-only"

// ReviewPlanRecord exists only after deterministic policy has made its final
// decision for an exact Revision. There is no pending plan state.
type ReviewPlanRecord struct {
	JobID      string             `json:"job_id"`
	Revision   string             `json:"revision"`
	Facts      policy.ChangeFacts `json:"facts"`
	Plan       policy.ReviewPlan  `json:"plan"`
	RecordedAt time.Time          `json:"recorded_at,omitempty"`
}

// MessageRecord is coding's read projection of one implementation Message.
// ProducerID is retained only to verify immutable Evidence provenance; Core
// owns the underlying AgentRun lifecycle and Harness identities.
type MessageRecord struct {
	Message       core.Message `json:"message"`
	SandboxID     string       `json:"sandbox_id"`
	InputRevision string       `json:"input_revision"`
	ProducerID    string       `json:"producer_id"`
	Outcome       string       `json:"outcome,omitempty"`
	Attention     string       `json:"attention,omitempty"`
	StartsTurn    bool         `json:"starts_turn"`
}

// ReviewRunView is coding's strict-review provenance projection. Harness
// identity remains here because checkout attestation and review Evidence must
// verify the independently isolated reviewer, but lifecycle state does not.
type ReviewRunView struct {
	ID              string       `json:"id"`
	JobID           string       `json:"job_id"`
	MessageID       string       `json:"message_id"`
	Harness         string       `json:"harness,omitempty"`
	ThreadID        string       `json:"thread_id,omitempty"`
	TurnID          string       `json:"turn_id,omitempty"`
	Outcome         string       `json:"outcome,omitempty"`
	Attention       string       `json:"attention,omitempty"`
	Role            string       `json:"role"`
	InputRevision   string       `json:"input_revision"`
	Capability      string       `json:"capability"`
	SandboxID       string       `json:"sandbox_id"`
	SubmissionNonce string       `json:"submission_nonce"`
	StartedAt       time.Time    `json:"started_at,omitempty"`
	FinishedAt      time.Time    `json:"finished_at,omitempty"`
	Request         core.Message `json:"request"`
	Sandbox         core.Sandbox `json:"sandbox"`
}

type reviewObservationPayload struct {
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
