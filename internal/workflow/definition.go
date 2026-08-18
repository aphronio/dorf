package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aphronio/dorf/internal/spine"
)

// ProviderCapability names an optional provider primitive beyond Dorf's
// baseline Sandbox and Harness contracts. Software dependencies belong to a
// repository setup script or custom image, not this vocabulary.
type ProviderCapability string

type Definition struct {
	Name                         spine.WorkflowName   `json:"name"`
	Revision                     string               `json:"revision"`
	RequiredProviderCapabilities []ProviderCapability `json:"required_provider_capabilities"`
}

type RuntimeProfile struct {
	SandboxProfile       string               `json:"sandbox_profile"`
	ProviderCapabilities []ProviderCapability `json:"provider_capabilities"`
}

func CodingToProposalDefinition() Definition {
	return Definition{
		Name: spine.WorkflowCodingToProposal, Revision: spine.CodingToProposalRevision,
	}
}

func CodebaseInvestigationDefinition() Definition {
	return Definition{
		Name: spine.WorkflowCodebaseInvestigation, Revision: spine.CodebaseInvestigationRevision,
	}
}

// ConfiguredRuntimeProfile identifies the selected provider profile. Current
// workflows need no optional provider primitive beyond the baseline contracts.
func ConfiguredRuntimeProfile(sandboxProfile string) RuntimeProfile {
	return RuntimeProfile{SandboxProfile: strings.TrimSpace(sandboxProfile)}
}

func (p RuntimeProfile) Require(definition Definition) error {
	available := make(map[ProviderCapability]bool, len(p.ProviderCapabilities))
	for _, capability := range p.ProviderCapabilities {
		available[capability] = true
	}
	var missing []string
	for _, capability := range definition.RequiredProviderCapabilities {
		if !available[capability] {
			missing = append(missing, string(capability))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("workflow %s revision %s requires missing provider capabilities: %s", definition.Name, definition.Revision, strings.Join(missing, ", "))
}

func definitionForJob(job spine.Job) (Definition, error) {
	switch job.Workflow {
	case spine.WorkflowCodingToProposal:
		definition := CodingToProposalDefinition()
		if job.WorkflowRevision != definition.Revision {
			return Definition{}, fmt.Errorf("Job pins unsupported %s workflow revision %q", job.Workflow, job.WorkflowRevision)
		}
		return definition, nil
	case spine.WorkflowCodebaseInvestigation:
		definition := CodebaseInvestigationDefinition()
		if job.WorkflowRevision != definition.Revision {
			return Definition{}, fmt.Errorf("Job pins unsupported %s workflow revision %q", job.Workflow, job.WorkflowRevision)
		}
		return definition, nil
	default:
		return Definition{}, fmt.Errorf("Job pins unsupported workflow %q", job.Workflow)
	}
}
