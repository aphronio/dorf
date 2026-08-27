package investigation

import "github.com/aphronio/dorf/internal/core"

const (
	Workflow         core.WorkflowName = "codebase-investigation"
	WorkflowRevision                   = "2"
	ReportPath                         = "REPORT.md"
)

type Source struct {
	JobID      string `json:"job_id,omitempty"`
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}
