package review

import (
	"strings"
	"testing"
)

func TestRolePromptRequestsConciseOrdinaryText(t *testing.T) {
	facts := ChangeFacts{Revision: "rev", BaseRevision: "base"}
	prompt := RolePrompt(RoleGeneral, facts)
	for _, required := range []string{"read-only access", "concise ordinary text", "advisory input"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("RolePrompt() lacks reviewer boundary %q: %s", required, prompt)
		}
	}
	for _, forbidden := range []string{"JSON", "FindingOutputContract", "affected_roles", "Return exactly one"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("RolePrompt() contains structured-output term %q: %s", forbidden, prompt)
		}
	}
	for _, fact := range []string{`"revision":"rev"`, `"base_revision":"base"`} {
		if !strings.Contains(prompt, fact) {
			t.Fatalf("RolePrompt() lacks immutable revision fact %q: %s", fact, prompt)
		}
	}
}
