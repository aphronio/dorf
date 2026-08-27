package investigation

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/aphronio/dorf/internal/core"
)

const (
	InitialAgentRole       = "investigate"
	InitialAgentCapability = "repository-read-report"
)

var (
	exactCommitOID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// Admission is the investigation workflow's complete immutable Job input.
type Admission struct {
	core.JobAdmission
	Source Source
}

func NormalizeAdmission(input Admission) (Admission, error) {
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	if input.Workflow != "" && input.Workflow != Workflow {
		return Admission{}, fmt.Errorf("codebase-investigation admission cannot use workflow %q", input.Workflow)
	}
	if input.WorkflowRevision != "" && input.WorkflowRevision != WorkflowRevision {
		return Admission{}, fmt.Errorf("codebase-investigation admission requires workflow revision %s", WorkflowRevision)
	}
	input.Workflow = Workflow
	input.WorkflowRevision = WorkflowRevision
	input.Source.JobID = ""
	input.Source.Repository = strings.TrimSpace(input.Source.Repository)
	input.Source.Revision = strings.TrimSpace(input.Source.Revision)
	if !exactCommitOID.MatchString(input.Source.Revision) {
		return Admission{}, fmt.Errorf("codebase-investigation source requires a lowercase full commit OID")
	}
	if err := validateRemoteRepository(input.Source.Repository); err != nil {
		return Admission{}, err
	}
	return input, nil
}

func validateRemoteRepository(repository string) error {
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(repository, "#") ||
		parsed.Opaque != "" || strings.Trim(parsed.Path, "/") == "" {
		return fmt.Errorf("remote investigation repository must be a credential-free HTTPS URL with a nonempty path and no query or fragment")
	}
	return nil
}
