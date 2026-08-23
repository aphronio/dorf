package investigation

import "github.com/aphronio/dorf/internal/core"

type SourceKind string

const (
	Workflow         core.WorkflowName = "codebase-investigation"
	WorkflowRevision                   = "2"
	ReportPath                         = "REPORT.md"

	SourceRemote    SourceKind = "remote"
	SourceGitBundle SourceKind = "git-bundle"
)

// Source is the immutable materialization input for one investigation. A
// retained bundle is input custody, not a workflow result.
type Source struct {
	JobID          string     `json:"job_id,omitempty"`
	Kind           SourceKind `json:"kind"`
	Repository     string     `json:"repository,omitempty"`
	Revision       string     `json:"revision"`
	BundleDigest   string     `json:"bundle_digest,omitempty"`
	BundleByteSize int64      `json:"bundle_byte_size,omitempty"`
}
