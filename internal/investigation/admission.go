package investigation

import "github.com/aphronio/dorf/internal/core"

// Admission is the investigation workflow's complete immutable Job input.
type Admission struct {
	core.JobAdmission
	Source Source
}
