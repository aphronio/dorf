package coding

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
)

const InitialAgentRole = "implement"

var exactCommitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

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

func NormalizeAdmission(input Admission) (Admission, error) {
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	if input.Workflow != "" && input.Workflow != Workflow {
		return Admission{}, fmt.Errorf("coding-to-proposal admission cannot use workflow %q", input.Workflow)
	}
	if input.WorkflowRevision != "" && input.WorkflowRevision != WorkflowRevision {
		return Admission{}, fmt.Errorf("coding-to-proposal admission requires workflow revision %s", WorkflowRevision)
	}
	input.Workflow = Workflow
	input.WorkflowRevision = WorkflowRevision
	input.Repository = strings.TrimSpace(input.Repository)
	input.Revision = strings.TrimSpace(input.Revision)
	input.Branch = strings.TrimSpace(input.Branch)
	input.GitHubRepository = strings.TrimSpace(input.GitHubRepository)
	input.GitHubInstallation = strings.TrimSpace(input.GitHubInstallation)
	input.BaseBranch = strings.TrimSpace(input.BaseBranch)
	if input.Repository == "" || input.Branch == "" || input.GitHubRepository == "" || input.GitHubInstallation == "" || input.BaseBranch == "" {
		return Admission{}, fmt.Errorf("coding-to-proposal admission requires workflow revision %s, canonical GitHub repository, installation, and explicit base branch", WorkflowRevision)
	}
	if !exactCommitOID.MatchString(input.Revision) {
		return Admission{}, fmt.Errorf("admitted revision must be a lowercase full commit OID (40 hex for SHA-1 or 64 hex for SHA-256)")
	}
	if err := githubapi.ValidateAuthority(input.Repository, input.GitHubRepository, input.GitHubInstallation, input.BaseBranch, input.Branch); err != nil {
		return Admission{}, err
	}
	return input, nil
}
