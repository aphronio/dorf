package workflow

import (
	"strings"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/profile"
)

// ProviderCapability names an optional provider primitive beyond Dorf's
// baseline Sandbox and Harness contracts.
type ProviderCapability = profile.Capability

// Presentation is optional human copy owned by one workflow revision. It is
// never persisted and never affects execution; missing entries fall back to
// readable names derived from the workflow's typed facts.
type Presentation struct {
	Operations map[string]string `json:"-"`
	AgentRoles map[string]string `json:"-"`
	Results    map[string]string `json:"-"`
}

type Definition struct {
	Name                         core.WorkflowName    `json:"name"`
	Revision                     string               `json:"revision"`
	RequiredProviderCapabilities []ProviderCapability `json:"required_provider_capabilities"`
	Presentation                 Presentation         `json:"-"`
}

type RuntimeProfile = profile.Runtime

func CodingToProposalDefinition() Definition {
	definition := coding.WorkflowDefinition()
	return Definition{
		Name: definition.Name, Revision: definition.Revision,
		RequiredProviderCapabilities: definition.RequiredProviderCapabilities,
		Presentation: Presentation{
			Operations: map[string]string{
				string(coding.WorkComplete):        "Complete",
				string(coding.WorkAttention):       "Needs attention",
				string(coding.WorkObserveRevision): "Inspecting implementation checkout",
				string(coding.WorkChooseReview):    "Choosing deterministic review",
				string(coding.WorkPublishProposal): "Publishing exact-Revision Proposal",
				string(coding.WorkObserveProposal): "Waiting for Proposal decision",
			},
			AgentRoles: map[string]string{"implement": "Implementation agent"},
		},
	}
}

func CodebaseInvestigationDefinition() Definition {
	return Definition{
		Name: investigation.Workflow, Revision: investigation.WorkflowRevision,
		Presentation: Presentation{
			Operations: map[string]string{
				string(InvestigationWorkComplete):  "Complete",
				string(InvestigationWorkAttention): "Needs attention",
				string(InvestigationWorkWaitInput): "Waiting for follow-up or cleanup",
			},
			AgentRoles: map[string]string{"investigate": "Investigator"},
			Results:    map[string]string{"investigation-draft": "Investigation draft"},
		},
	}
}

func (d Definition) OperationLabel(kind, fallback string) string {
	return presentationLabel(d.Presentation.Operations, kind, fallback)
}

func (d Definition) ActionLabel(kind core.ActionKind) string {
	switch kind {
	case core.ActionSandboxCreate:
		return "Provisioning Sandbox"
	case gitworkspace.ActionRepositoryClone:
		return "Cloning repository"
	case investigation.ActionRepositoryRestore:
		return "Restoring retained repository"
	case coding.ActionRepositoryPush:
		return "Publishing Revision"
	case coding.ActionGitHubPullRequest:
		return "Creating pull request"
	case coding.ActionReviewCheckout:
		return "Preparing reviewer checkout"
	case core.ActionRouteCreate:
		return "Connecting model access"
	case core.ActionRouteRevoke:
		return "Revoking model access"
	case core.ActionSandboxDelete:
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
