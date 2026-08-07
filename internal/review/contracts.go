package review

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	TriageOutputContract  = `{"roles":["allowlisted-role"],"rationale":"bounded rationale"}`
	FindingOutputContract = `{"material":false,"summary":"bounded summary","rationale":"bounded rationale","affected_roles":[],"affected_checks":[]}`
)

type TriageOutput struct {
	Roles     []Role `json:"roles"`
	Rationale string `json:"rationale"`
}

type FindingOutput struct {
	Material       bool     `json:"material"`
	Summary        string   `json:"summary"`
	Rationale      string   `json:"rationale"`
	AffectedRoles  []Role   `json:"affected_roles"`
	AffectedChecks []string `json:"affected_checks"`
}

type triageOutputWire struct {
	Roles     *[]Role `json:"roles"`
	Rationale string  `json:"rationale"`
}

type findingOutputWire struct {
	Material       *bool     `json:"material"`
	Summary        string    `json:"summary"`
	Rationale      string    `json:"rationale"`
	AffectedRoles  *[]Role   `json:"affected_roles"`
	AffectedChecks *[]string `json:"affected_checks"`
}

func TriagePrompt(facts ChangeFacts) string {
	encoded, _ := json.Marshal(facts)
	return "You are the bounded Dorf review-triage AgentRun. Read only the immutable Revision and classify only genuinely unknown change paths. You have no coordination, write, check, readiness, or cleanup authority. You may add zero or more Roles from this exact allowlist: " + roleList() + ". Return exactly one JSON object matching " + TriageOutputContract + ". ChangeFacts: " + string(encoded)
}

func RolePrompt(role Role, facts ChangeFacts, declaredChecks []string) string {
	encoded, _ := json.Marshal(facts)
	checks, _ := json.Marshal(declaredChecks)
	return fmt.Sprintf("You are the bounded Dorf %s review AgentRun. Review only exact immutable Revision %s against base %s. Your filesystem capability is read-only. Report at most one material finding; your output is a claim and never satisfies Checks or controls readiness. affected_roles may contain only your own Role. A material finding must list every name in this exact declared Check list because a repair Revision invalidates all exact-Revision Check proof: %s. A non-material result must list none. Return exactly one JSON object matching %s. ChangeFacts: %s", role, facts.Revision, facts.BaseRevision, checks, FindingOutputContract, encoded)
}

func ParseTriageOutput(raw string) (TriageOutput, error) {
	var wire triageOutputWire
	if err := strictJSON(raw, &wire); err != nil {
		return TriageOutput{}, fmt.Errorf("invalid bounded triage output: %w", err)
	}
	if wire.Roles == nil {
		return TriageOutput{}, fmt.Errorf("invalid bounded triage output: roles is required and cannot be null")
	}
	output := TriageOutput{Roles: *wire.Roles, Rationale: wire.Rationale}
	if strings.TrimSpace(output.Rationale) == "" || len(output.Rationale) > 4096 || len(output.Roles) > len(allowedRoles) {
		return TriageOutput{}, fmt.Errorf("invalid bounded triage rationale or Role count")
	}
	seen := map[Role]bool{}
	for _, role := range output.Roles {
		if !Allowed(role) || seen[role] {
			return TriageOutput{}, fmt.Errorf("triage returned invalid, unsafe, or duplicate Role %q", role)
		}
		seen[role] = true
	}
	return output, nil
}

func ParseFindingOutput(raw string, role Role, declaredChecks []string) (FindingOutput, error) {
	var wire findingOutputWire
	if err := strictJSON(raw, &wire); err != nil {
		return FindingOutput{}, fmt.Errorf("invalid bounded review output: %w", err)
	}
	if wire.Material == nil || wire.AffectedRoles == nil || wire.AffectedChecks == nil {
		return FindingOutput{}, fmt.Errorf("invalid bounded review output: material, affected_roles, and affected_checks are required and cannot be null")
	}
	output := FindingOutput{Material: *wire.Material, Summary: wire.Summary, Rationale: wire.Rationale, AffectedRoles: *wire.AffectedRoles, AffectedChecks: *wire.AffectedChecks}
	if strings.TrimSpace(output.Summary) == "" || strings.TrimSpace(output.Rationale) == "" || len(output.Summary) > 4096 || len(output.Rationale) > 16384 {
		return FindingOutput{}, fmt.Errorf("review summary or rationale is empty or exceeds its bound")
	}
	if len(output.AffectedRoles) > 1 || len(output.AffectedChecks) > len(declaredChecks) {
		return FindingOutput{}, fmt.Errorf("review output exceeds targeted proof bounds")
	}
	for _, affected := range output.AffectedRoles {
		if affected != role {
			return FindingOutput{}, fmt.Errorf("Role %q cannot invalidate review Role %q", role, affected)
		}
	}
	allowedChecks := map[string]bool{}
	for _, name := range declaredChecks {
		allowedChecks[name] = true
	}
	seen := map[string]bool{}
	for _, name := range output.AffectedChecks {
		if !allowedChecks[name] || seen[name] {
			return FindingOutput{}, fmt.Errorf("review returned invalid or duplicate affected Check %q", name)
		}
		seen[name] = true
	}
	if !output.Material && (len(output.AffectedRoles) != 0 || len(output.AffectedChecks) != 0) {
		return FindingOutput{}, fmt.Errorf("non-material claim cannot invalidate proof")
	}
	if output.Material && len(output.AffectedChecks) != len(declaredChecks) {
		return FindingOutput{}, fmt.Errorf("material claim must invalidate every exact-Revision declared Check")
	}
	if output.Material && len(output.AffectedRoles) == 0 {
		output.AffectedRoles = []Role{role}
	}
	return output, nil
}

func strictJSON(raw string, target any) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return fmt.Errorf("output is empty or exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("output has trailing content")
	}
	return nil
}

func roleList() string {
	values := make([]string, 0, len(allowedRoles))
	for _, role := range allowedRoles {
		values = append(values, string(role))
	}
	return strings.Join(values, ", ")
}
