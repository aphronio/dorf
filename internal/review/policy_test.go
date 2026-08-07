package review

import (
	"reflect"
	"strings"
	"testing"
)

func TestReviewPolicyTable(t *testing.T) {
	base, revision := strings.Repeat("a", 40), strings.Repeat("b", 40)
	tests := []struct {
		name      string
		paths     []string
		perf      bool
		requested []Role
		decision  string
		roles     []Role
	}{
		{"green docs only", []string{"README.md", "docs/review.md"}, false, nil, "no-review", nil},
		{"docs web marker stays documentation", []string{"docs/web/guide.md"}, false, nil, "no-review", nil},
		{"docs auth marker stays documentation", []string{"docs/auth/README.md"}, false, nil, "no-review", nil},
		{"browser UI", []string{"web/app.tsx"}, false, nil, "selected", []Role{RoleBrowserUI}},
		{"authentication authority", []string{"internal/auth/policy.go"}, false, nil, "selected", []Role{RoleAuthAuthority}},
		{"declared performance retains unknown triage", []string{"internal/cache/cache.go"}, true, nil, "triage", []Role{RolePerformance}},
		{"covered UI plus declared performance", []string{"web/app.tsx"}, true, nil, "selected", []Role{RoleBrowserUI, RolePerformance}},
		{"docs markers plus real UI and auth", []string{"docs/web/guide.md", "docs/auth/README.md", "web/app.tsx", "internal/auth/policy.go"}, false, nil, "selected", []Role{RoleAuthAuthority, RoleBrowserUI}},
		{"mandatory plus implementation request", []string{"web/app.tsx"}, false, []Role{RoleCriticalBoundary}, "selected", []Role{RoleBrowserUI, RoleCriticalBoundary}},
		{"unknown", []string{"internal/spine/service.go"}, false, nil, "triage", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts, err := FactsFromPaths(base, revision, test.paths, true, test.perf)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReviewPolicy(facts, test.requested)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != test.decision || !reflect.DeepEqual(got.Roles, test.roles) {
				t.Fatalf("plan=%#v want decision=%s roles=%v", got, test.decision, test.roles)
			}
			repeated, err := ReviewPolicy(facts, test.requested)
			if err != nil || !reflect.DeepEqual(got, repeated) {
				t.Fatalf("repeated policy changed: %#v err=%v", repeated, err)
			}
		})
	}
}

func TestMandatoryRulesCannotBeWaivedAndUnsafeRequestsStop(t *testing.T) {
	facts, err := FactsFromPaths(strings.Repeat("a", 40), strings.Repeat("b", 40), []string{"auth/login.go"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ReviewPolicy(facts, nil)
	if err != nil || !reflect.DeepEqual(plan.Roles, []Role{RoleAuthAuthority}) {
		t.Fatalf("mandatory plan=%#v err=%v", plan, err)
	}
	if _, err := ReviewPolicy(facts, []Role{"coordinator"}); err == nil || !strings.Contains(err.Error(), "invalid or unsafe") {
		t.Fatalf("unsafe request error=%v", err)
	}
}

func TestTriageCanOnlyAddAllowlistedRolesOnce(t *testing.T) {
	facts, _ := FactsFromPaths(strings.Repeat("a", 40), strings.Repeat("b", 40), []string{"internal/spine/service.go"}, true, false)
	plan, _ := ReviewPolicy(facts, nil)
	resolved, err := AddTriage(plan, []Role{RoleCriticalBoundary}, "durable state transition needs boundary review")
	if err != nil || resolved.Decision != "selected" || resolved.NeedsTriage || !reflect.DeepEqual(resolved.Roles, []Role{RoleCriticalBoundary}) {
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
	plan, _ := ReviewPolicy(facts, []Role{RoleBrowserUI, RolePerformance, RoleCriticalBoundary})
	if plan.NeedsTriage || !reflect.DeepEqual(plan.Roles, []Role{RoleAuthAuthority, RoleBrowserUI, RoleCriticalBoundary, RolePerformance}) {
		t.Fatalf("initial requested plan=%#v", plan)
	}
	originalRoles := append([]Role(nil), plan.Roles...)
	originalReasons := append([]Reason(nil), plan.Reasons...)
	targeted, err := TargetedReverification(plan, []Role{RoleCriticalBoundary})
	wantReasons := []Reason{
		{Role: RoleAuthAuthority, Source: "mandatory", Detail: "authentication or authority paths changed"},
		{Role: RoleCriticalBoundary, Source: "accepted-finding", Detail: "accepted material finding invalidated this Role's claim"},
	}
	if err != nil || targeted.NeedsTriage || !reflect.DeepEqual(targeted.Roles, []Role{RoleAuthAuthority, RoleCriticalBoundary}) || !reflect.DeepEqual(targeted.Reasons, wantReasons) {
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
