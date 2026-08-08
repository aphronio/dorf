package review

import (
	"reflect"
	"testing"
)

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

func TestFindingOutputRequiresExplicitAuthorityAndProofFields(t *testing.T) {
	tests := []struct {
		name           string
		raw            string
		declaredChecks []string
	}{
		{name: "only summary and rationale", raw: `{"summary":"clear","rationale":"no issue"}`},
		{name: "material omitted", raw: `{"summary":"clear","rationale":"no issue","affected_roles":[],"affected_checks":[]}`},
		{name: "affected roles omitted", raw: `{"material":false,"summary":"clear","rationale":"no issue","affected_checks":[]}`},
		{name: "affected checks omitted", raw: `{"material":false,"summary":"clear","rationale":"no issue","affected_roles":[]}`},
		{name: "material null", raw: `{"material":null,"summary":"clear","rationale":"no issue","affected_roles":[],"affected_checks":[]}`},
		{name: "affected roles null", raw: `{"material":false,"summary":"clear","rationale":"no issue","affected_roles":null,"affected_checks":[]}`},
		{name: "affected checks null", raw: `{"material":false,"summary":"clear","rationale":"no issue","affected_roles":[],"affected_checks":null}`},
		{name: "material missing roles", raw: `{"material":true,"summary":"finding","rationale":"repair","affected_checks":["check"]}`, declaredChecks: []string{"check"}},
		{name: "material missing checks with none declared", raw: `{"material":true,"summary":"finding","rationale":"repair","affected_roles":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if output, err := ParseFindingOutput(test.raw, RoleBrowserUI, test.declaredChecks); err == nil {
				t.Fatalf("accepted incomplete finding output: %#v", output)
			}
		})
	}
}

func TestFindingOutputAcceptsCompleteClaimsAndNormalizesMaterialRole(t *testing.T) {
	nonMaterial, err := ParseFindingOutput(`{"material":false,"summary":"clear","rationale":"no issue","affected_roles":[],"affected_checks":[]}`, RoleBrowserUI, nil)
	if err != nil || nonMaterial.Material || len(nonMaterial.AffectedRoles) != 0 || len(nonMaterial.AffectedChecks) != 0 {
		t.Fatalf("complete non-material output=%#v err=%v", nonMaterial, err)
	}
	material, err := ParseFindingOutput(`{"material":true,"summary":"finding","rationale":"repair","affected_roles":[],"affected_checks":["check"]}`, RoleBrowserUI, []string{"check"})
	if err != nil || !material.Material || !reflect.DeepEqual(material.AffectedRoles, []Role{RoleBrowserUI}) || !reflect.DeepEqual(material.AffectedChecks, []string{"check"}) {
		t.Fatalf("complete material output=%#v err=%v", material, err)
	}
}

func TestTriageOutputRequiresExplicitRolesButAllowsEmptySelection(t *testing.T) {
	for _, raw := range []string{
		`{"rationale":"no specialized review needed"}`,
		`{"roles":null,"rationale":"no specialized review needed"}`,
	} {
		if output, err := ParseTriageOutput(raw); err == nil {
			t.Fatalf("accepted incomplete triage output: %#v", output)
		}
	}
	output, err := ParseTriageOutput(`{"roles":[],"rationale":"no specialized review needed"}`)
	if err != nil || len(output.Roles) != 0 || output.Rationale != "no specialized review needed" {
		t.Fatalf("explicit empty triage output=%#v err=%v", output, err)
	}
}
