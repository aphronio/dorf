package investigation

import (
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

func actionLabel(kind core.ActionKind) string {
	switch kind {
	case core.ActionSandboxCreate:
		return "Provisioning Sandbox"
	case gitworkspace.ActionRepositoryClone:
		return "Cloning repository"
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

func humanizeIdentifier(value string) string {
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(strings.TrimSpace(value)))
	if len(words) == 0 {
		return "Unknown"
	}
	words[0] = strings.ToUpper(words[0][:1]) + words[0][1:]
	return strings.Join(words, " ")
}
