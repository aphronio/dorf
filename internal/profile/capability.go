package profile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aphronio/dorf/internal/core"
)

// Capability names an optional provider primitive beyond Dorf's baseline
// Sandbox and Harness contracts.
type Capability string

// Runtime is the provider capability set resolved for one named Sandbox
// profile. The durable profile definition remains in Core custody.
type Runtime struct {
	SandboxProfile       string       `json:"sandbox_profile"`
	ProviderCapabilities []Capability `json:"provider_capabilities"`
}

func (p Runtime) Require(workflow core.WorkflowName, revision string, required []Capability) error {
	if strings.TrimSpace(p.SandboxProfile) == "" {
		return fmt.Errorf("workflow %s revision %s requires a named Sandbox profile", workflow, revision)
	}
	available := make(map[Capability]bool, len(p.ProviderCapabilities))
	for _, capability := range p.ProviderCapabilities {
		available[capability] = true
	}
	var missing []string
	for _, capability := range required {
		if !available[capability] {
			missing = append(missing, string(capability))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("workflow %s revision %s requires missing provider capabilities: %s", workflow, revision, strings.Join(missing, ", "))
}
