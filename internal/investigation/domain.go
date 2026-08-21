package investigation

import (
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

type SourceKind string

const (
	Workflow         core.WorkflowName = "codebase-investigation"
	WorkflowRevision                   = "2"

	SourceRemote    SourceKind = "remote"
	SourceGitBundle SourceKind = "git-bundle"
)

// Source is the immutable materialization input for one investigation. A
// retained bundle is input custody, not an output Artifact.
type Source struct {
	JobID          string     `json:"job_id,omitempty"`
	Kind           SourceKind `json:"kind"`
	Repository     string     `json:"repository,omitempty"`
	Revision       string     `json:"revision"`
	BundleDigest   string     `json:"bundle_digest,omitempty"`
	BundleByteSize int64      `json:"bundle_byte_size,omitempty"`
}

type Draft struct {
	JobID      string    `json:"job_id"`
	MessageID  string    `json:"message_id"`
	ArtifactID string    `json:"artifact_id"`
	CreatedAt  time.Time `json:"created_at"`
}

func DraftArtifactName(sequence int64) string {
	return fmt.Sprintf("report-%04d.md", sequence)
}
