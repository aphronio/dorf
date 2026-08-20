package coding

import (
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
	"github.com/aphronio/dorf/internal/profile"
)

// Definition is the coding workflow revision's requirements and optional
// human presentation. It never controls Core execution or durable state.
type Definition struct {
	Name                         core.WorkflowName    `json:"name"`
	Revision                     string               `json:"revision"`
	RequiredProviderCapabilities []profile.Capability `json:"required_provider_capabilities"`
}

func WorkflowDefinition() Definition {
	return Definition{Name: Workflow, Revision: WorkflowRevision}
}

func (Definition) OperationLabel(kind, fallback string) string {
	switch WorkKind(kind) {
	case WorkComplete:
		return "Complete"
	case WorkAttention:
		return "Needs attention"
	case WorkObserveRevision:
		return "Inspecting implementation checkout"
	case WorkChooseReview:
		return "Choosing deterministic review"
	case WorkPublishProposal:
		return "Publishing exact-Revision Proposal"
	case WorkObserveProposal:
		return "Waiting for Proposal decision"
	default:
		return fallback
	}
}

func (Definition) ActionLabel(kind core.ActionKind) string {
	switch kind {
	case core.ActionSandboxCreate:
		return "Provisioning Sandbox"
	case gitworkspace.ActionRepositoryClone:
		return "Cloning repository"
	case ActionRepositoryPush:
		return "Publishing Revision"
	case ActionGitHubPullRequest:
		return "Creating pull request"
	case ActionReviewCheckout:
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

func (Definition) AgentRoleLabel(role string) string {
	if role == "implement" {
		return "Implementation agent"
	}
	return humanizeIdentifier(role)
}

func (Definition) ResultLabel(kind string) string { return humanizeIdentifier(kind) }

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
