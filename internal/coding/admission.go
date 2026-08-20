package coding

import "github.com/aphronio/dorf/internal/core"

// Admission is the coding workflow's complete immutable Job input.
type Admission struct {
	core.JobAdmission
	Repository         string
	Revision           string
	Branch             string
	GitHubRepository   string
	GitHubInstallation string
	BaseBranch         string
}
