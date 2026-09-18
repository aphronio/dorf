package sandbox

import "context"

// ReviewMetadata binds an isolated review Sandbox to the exact admitted run
// and Revision. It is additional attestation, never cleanup identity.
type ReviewMetadata struct {
	JobID          string `json:"job_id"`
	AgentRunID     string `json:"agent_run_id"`
	Revision       string `json:"revision"`
	OwnershipNonce string `json:"ownership_nonce"`
}

// ReviewAttester is required only by strict-review Harness operations.
// Ordinary Sandbox execution does not require review metadata or attestation.
type ReviewAttester interface {
	AttestReview(context.Context, Ownership, ReviewMetadata) error
}
