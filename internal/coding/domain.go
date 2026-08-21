package coding

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

const (
	Workflow         core.WorkflowName = "coding-to-proposal"
	WorkflowRevision                   = "3"
)

// Job is the coding-to-proposal workflow's complete typed state. Its embedded
// Core Job carries only shared custody and lifecycle facts.
type Job struct {
	core.Job
	Repository         string `json:"repository"`
	StartingRevision   string `json:"starting_revision"`
	Revision           string `json:"revision"`
	Branch             string `json:"branch"`
	GitHubRepository   string `json:"github_repository"`
	GitHubInstallation string `json:"github_installation_id"`
	BaseBranch         string `json:"base_branch"`
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

type Proposal struct {
	JobID            string `json:"job_id"`
	Number           int64  `json:"pr_number"`
	URL              string `json:"pr_url"`
	ProposedRevision string `json:"proposed_revision"`
	BodyDigest       string `json:"body_digest"`
}

type OutcomeKind string

const (
	OutcomeAccepted  OutcomeKind = "accepted"
	OutcomeRejected  OutcomeKind = "rejected"
	OutcomeAbandoned OutcomeKind = "abandoned"
)

type Outcome struct {
	JobID          string      `json:"job_id"`
	Kind           OutcomeKind `json:"outcome"`
	ObservedState  string      `json:"observed_state"`
	ObservedMerged bool        `json:"observed_merged"`
	MergeCommitOID string      `json:"merge_commit_oid,omitempty"`
	ObservedAt     time.Time   `json:"observed_at"`
}

func ReviewRequestFromID(revision, role string) string {
	return "review:" + revision + ":" + role
}

func ReviewRequestMessageID(jobID, revision, role string) string {
	return core.MessageID(jobID, core.MessageFromWorkflow, ReviewRequestFromID(revision, role))
}

func ReviewPolicyAttentionSource(revision string) string {
	return "review-policy:" + revision
}

func ReviewSandboxName(jobID, runID string) string {
	return core.NamedSandboxID(jobID, runID)
}

func codingDigest(value string, length int) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:length]
}
