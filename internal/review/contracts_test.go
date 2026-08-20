package review

import (
	"strings"
	"testing"
)

func TestRolePromptRequestsConciseOrdinaryText(t *testing.T) {
	facts := ChangeFacts{Revision: "rev", BaseRevision: "base"}
	prompt := RolePrompt(RoleGeneral, facts)
	if containsAny(prompt, "JSON", "FindingOutputContract", "affected_roles", "Return exactly one") {
		t.Fatalf("prompt still describes structured output: %s", prompt)
	}
	if !containsAny(prompt, "concise ordinary text", "read-only", "advisory") {
		t.Fatalf("prompt lost reviewer boundaries: %s", prompt)
	}
}

func containsAny(s string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}
