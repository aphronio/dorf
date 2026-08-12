package review

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RolePrompt gives a read-only reviewer the immutable context it needs. The
// response is deliberately ordinary prose: reviewer text is advisory Message
// input, not a protocol that Dorf parses or treats as authority.
func RolePrompt(role Role, facts ChangeFacts, declaredChecks []string) string {
	encoded, _ := json.Marshal(facts)
	checks := strings.Join(declaredChecks, ", ")
	if checks == "" {
		checks = "none declared"
	}
	return fmt.Sprintf("You are the bounded Dorf %s review AgentRun. Review exact immutable Revision %s against base %s using read-only access. Respond with concise ordinary text: state whether you found a material issue, explain the issue briefly when present, and mention relevant Checks (%s). Your response is advisory input to the next implementation AgentRun; it does not satisfy Checks or control readiness. ChangeFacts: %s", role, facts.Revision, facts.BaseRevision, checks, encoded)
}
