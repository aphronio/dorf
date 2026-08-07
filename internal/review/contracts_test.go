package review

import "testing"

func TestBoundedOutputsRejectAuthorityExpansion(t *testing.T) {
	if _, err := ParseTriageOutput(`{"roles":["coordinator"],"rationale":"coordinate everything"}`); err == nil {
		t.Fatal("triage invented a Role")
	}
	if _, err := ParseFindingOutput(`{"material":true,"summary":"x","rationale":"x","affected_roles":["performance"],"affected_checks":["check"]}`, RoleBrowserUI, []string{"check"}); err == nil {
		t.Fatal("review invalidated another Role")
	}
	if _, err := ParseFindingOutput(`{"material":false,"summary":"clear","rationale":"clear","affected_roles":[],"affected_checks":[]}`, RoleBrowserUI, []string{"check"}); err != nil {
		t.Fatal(err)
	}
}
