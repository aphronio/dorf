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

// Presentation is optional human copy owned by one workflow revision. It is
// never persisted and never affects execution; missing entries fall back to
// readable names derived from the workflow's typed facts.
type Presentation struct {
	Operations map[string]string `json:"-"`
	AgentRoles map[string]string `json:"-"`
	Results    map[string]string `json:"-"`
}

type Definition struct {
	Name                         spine.WorkflowName   `json:"name"`
	Revision                     string               `json:"revision"`
	RequiredProviderCapabilities []ProviderCapability `json:"required_provider_capabilities"`
	Presentation                 Presentation         `json:"-"`
}

type RuntimeProfile struct {
	SandboxProfile       string               `json:"sandbox_profile"`
	ProviderCapabilities []ProviderCapability `json:"provider_capabilities"`
}

func CodingToProposalDefinition() Definition {
	return Definition{
		Name: spine.WorkflowCodingToProposal, Revision: spine.CodingToProposalRevision,
		Presentation: Presentation{
			Operations: map[string]string{
				string(WorkComplete):        "Complete",
				string(WorkAttention):       "Needs attention",
				string(WorkSetupRepository): "Running repository setup",
				string(WorkObserveRevision): "Inspecting implementation checkout",
				string(WorkRunChecks):       "Running deterministic Checks",
				string(WorkChooseReview):    "Choosing deterministic review",
				string(WorkPublishProposal): "Publishing exact-Revision Proposal",
				string(WorkObserveProposal): "Waiting for Proposal decision",
			},
			AgentRoles: map[string]string{"implement": "Implementation agent"},
		},
	}
}

func CodebaseInvestigationDefinition() Definition {
	return Definition{
		Name: spine.WorkflowCodebaseInvestigation, Revision: spine.CodebaseInvestigationRevision,
		Presentation: Presentation{
			Operations: map[string]string{
				string(InvestigationWorkComplete):  "Complete",
				string(InvestigationWorkAttention): "Needs attention",
			},
			AgentRoles: map[string]string{"investigate": "Investigator"},
			Results:    map[string]string{"investigation-report": "Investigation report"},
		},
	}
}

func (d Definition) OperationLabel(kind, fallback string) string {
	return presentationLabel(d.Presentation.Operations, kind, fallback)
}

func (d Definition) ActionLabel(kind spine.ActionKind) string {
	switch kind {
	case spine.ActionSandboxCreate:
		return "Provisioning Sandbox"
	case spine.ActionRepositoryClone:
		return "Cloning repository"
	case spine.ActionRepositorySetup:
		return "Setting up repository"
	case spine.ActionRepositoryPush:
		return "Publishing Revision"
	case spine.ActionGitHubPullRequest:
		return "Creating pull request"
	case spine.ActionReviewCheckout:
		return "Preparing reviewer checkout"
	case spine.ActionRouteCreate:
		return "Connecting model access"
	case spine.ActionRouteRevoke:
		return "Revoking model access"
	case spine.ActionSandboxDelete:
		return "Deleting Sandbox"
	default:
		return humanizeIdentifier(string(kind))
	}
}

func (d Definition) AgentRoleLabel(role string) string {
	return presentationLabel(d.Presentation.AgentRoles, role, humanizeIdentifier(role))
}

func (d Definition) ResultLabel(kind string) string {
	return presentationLabel(d.Presentation.Results, kind, humanizeIdentifier(kind))
}

func presentationLabel(values map[string]string, key, fallback string) string {
	if label := strings.TrimSpace(values[key]); label != "" {
		return label
	}
	return strings.TrimSpace(fallback)
}

func humanizeIdentifier(value string) string {
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(strings.TrimSpace(value)))
	if len(words) == 0 {
		return "Unknown"
	}
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}

func lowerFirst(value string) string {
	if value == "" {
		return value
	}
	return strings.ToLower(value[:1]) + value[1:]
}

func (p RuntimeProfile) Require(definition Definition) error {
	if strings.TrimSpace(p.SandboxProfile) == "" {
		return fmt.Errorf("workflow %s revision %s requires a named Sandbox profile", definition.Name, definition.Revision)
	}
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
