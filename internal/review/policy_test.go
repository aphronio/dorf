package review

import (
	"reflect"
	"strings"
	"testing"
)

func TestReviewPolicyTable(t *testing.T) {
	base, revision := strings.Repeat("a", 40), strings.Repeat("b", 40)
	tests := []struct {
		name     string
		paths    []string
		perf     bool
		decision string
		roles    []Role
	}{
		{"green docs only", []string{"README.md", "docs/review.md"}, false, "no-review", nil},
		{"docs web marker stays documentation", []string{"docs/web/guide.md"}, false, "no-review", nil},
		{"docs auth marker stays documentation", []string{"docs/auth/README.md"}, false, "no-review", nil},
		{"browser UI", []string{"web/app.tsx"}, false, "selected", []Role{RoleBrowserUI}},
		{"authentication authority", []string{"internal/auth/policy.go"}, false, "selected", []Role{RoleAuthAuthority}},
		{"declared performance retains unknown triage", []string{"internal/cache/cache.go"}, true, "triage", []Role{RolePerformance}},
		{"covered UI plus declared performance", []string{"web/app.tsx"}, true, "selected", []Role{RoleBrowserUI, RolePerformance}},
		{"docs markers plus real UI and auth", []string{"docs/web/guide.md", "docs/auth/README.md", "web/app.tsx", "internal/auth/policy.go"}, false, "selected", []Role{RoleAuthAuthority, RoleBrowserUI}},
		{"unknown", []string{"internal/spine/service.go"}, false, "triage", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts, err := FactsFromPaths(base, revision, test.paths, true, test.perf)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReviewPolicy(facts)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != test.decision || !reflect.DeepEqual(got.Roles, test.roles) {
				t.Fatalf("plan=%#v want decision=%s roles=%v", got, test.decision, test.roles)
			}
			repeated, err := ReviewPolicy(facts)
			if err != nil || !reflect.DeepEqual(got, repeated) {
				t.Fatalf("repeated policy changed: %#v err=%v", repeated, err)
			}
		})
	}
}

func TestMandatoryRulesAreDeterministic(t *testing.T) {
	facts, err := FactsFromPaths(strings.Repeat("a", 40), strings.Repeat("b", 40), []string{"auth/login.go"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ReviewPolicy(facts)
	if err != nil || !reflect.DeepEqual(plan.Roles, []Role{RoleAuthAuthority}) {
		t.Fatalf("mandatory plan=%#v err=%v", plan, err)
	}
}

func TestTriageCanOnlyAddAllowlistedRolesOnce(t *testing.T) {
	facts, _ := FactsFromPaths(strings.Repeat("a", 40), strings.Repeat("b", 40), []string{"internal/spine/service.go"}, true, false)
	plan, _ := ReviewPolicy(facts)
	resolved, err := AddTriage(plan, []Role{RoleCriticalBoundary}, "durable state transition needs boundary review")
	if err != nil || resolved.Decision != "selected" || !reflect.DeepEqual(resolved.Roles, []Role{RoleCriticalBoundary}) {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if _, err := AddTriage(resolved, nil, "again"); err == nil {
		t.Fatal("accepted a second triage")
	}
	if _, err := AddTriage(plan, []Role{"coordinator"}, "unsafe"); err == nil {
		t.Fatal("accepted unsafe triage Role")
	}
}

func TestTargetedReverificationKeepsMandatoryFloorWithoutBroadTriage(t *testing.T) {
	facts, _ := FactsFromPaths(strings.Repeat("a", 40), strings.Repeat("b", 40), []string{"internal/auth/session.go"}, true, false)
	plan, _ := ReviewPolicy(facts)
	if !reflect.DeepEqual(plan.Roles, []Role{RoleAuthAuthority}) {
		t.Fatalf("initial mandatory plan=%#v", plan)
	}
	originalRoles := append([]Role(nil), plan.Roles...)
	originalReasons := append([]Reason(nil), plan.Reasons...)
	targeted, err := TargetedReverification(plan, []Role{RoleCriticalBoundary})
	wantReasons := []Reason{
		{Role: RoleAuthAuthority, Source: "mandatory", Detail: "authentication or authority paths changed"},
		{Role: RoleCriticalBoundary, Source: "accepted-finding", Detail: "accepted material finding invalidated this Role's claim"},
	}
	if err != nil || !reflect.DeepEqual(targeted.Roles, []Role{RoleAuthAuthority, RoleCriticalBoundary}) || !reflect.DeepEqual(targeted.Reasons, wantReasons) {
		t.Fatalf("targeted plan=%#v err=%v", targeted, err)
	}
	if !reflect.DeepEqual(plan.Roles, originalRoles) || !reflect.DeepEqual(plan.Reasons, originalReasons) {
		t.Fatalf("targeted policy mutated original plan: %#v", plan)
	}
	overlap, err := TargetedReverification(plan, []Role{RoleAuthAuthority})
	if err != nil || !reflect.DeepEqual(overlap.Roles, []Role{RoleAuthAuthority}) || len(overlap.Reasons) != 2 || overlap.Reasons[0].Source != "accepted-finding" || overlap.Reasons[1].Source != "mandatory" {
		t.Fatalf("overlapping mandatory target=%#v err=%v", overlap, err)
	}
}
