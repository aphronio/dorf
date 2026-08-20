package spine

import (
	"fmt"
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

// ReviewRuns derives the concrete reviewer execution aggregates from the
// Message + AgentRun relationship and Job-owned Sandboxes. The returned views
// are disposable; each durable fact remains represented once in Delivery or
// Sandbox.
func ReviewRuns(deliveries []Delivery, sandboxes []Sandbox) ([]ReviewRunView, error) {
	byID := make(map[string]Sandbox, len(sandboxes))
	for _, sandbox := range sandboxes {
		byID[sandbox.ID] = sandbox
	}
	runs := make([]ReviewRunView, 0)
	for _, delivery := range deliveries {
		run := delivery.AgentRun
		if run.Role == "implement" {
			continue
		}
		if run.InputRevision == "" {
			return nil, fmt.Errorf("review AgentRun %s has no exact input Revision", run.ID)
		}
		request := delivery.Message
		if request.ID == "" || request.ID != run.MessageID || request.JobID == "" || request.JobID != run.JobID {
			return nil, fmt.Errorf("review AgentRun %s has no exact input Message relationship", run.ID)
		}
		if run.Capability != ReviewReadOnlyCapability {
			return nil, fmt.Errorf("review AgentRun %s has capability %q, want %q", run.ID, run.Capability, ReviewReadOnlyCapability)
		}
		sandbox, ok := byID[run.SandboxID]
		if !ok || sandbox.ID == "" || sandbox.JobID != run.JobID {
			return nil, fmt.Errorf("review AgentRun %s has no exact Job-owned Sandbox %s", run.ID, run.SandboxID)
		}
		runs = append(runs, ReviewRunView{AgentRun: run, Request: request, Sandbox: sandbox})
	}
	return runs, nil
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
