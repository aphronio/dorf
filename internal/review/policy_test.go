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
		{"declared performance plus unknown selects general", []string{"internal/cache/cache.go"}, true, "selected", []Role{RoleGeneral, RolePerformance}},
		{"covered UI plus declared performance", []string{"web/app.tsx"}, true, "selected", []Role{RoleBrowserUI, RolePerformance}},
		{"docs markers plus real UI and auth", []string{"docs/web/guide.md", "docs/auth/README.md", "web/app.tsx", "internal/auth/policy.go"}, false, "selected", []Role{RoleAuthAuthority, RoleBrowserUI}},
		{"unknown", []string{"internal/spine/service.go"}, false, "selected", []Role{RoleGeneral}},
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

func TestFactsFromPathsDeduplicatesAndSortsPaths(t *testing.T) {
	facts, err := FactsFromPaths(
		strings.Repeat("a", 40),
		strings.Repeat("b", 40),
		[]string{"web/z.tsx", "README.md", "web/z.tsx", "docs/guide.md", "README.md"},
		true,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"README.md", "docs/guide.md", "web/z.tsx"}
	if !reflect.DeepEqual(facts.Paths, want) {
		t.Fatalf("paths=%v want %v", facts.Paths, want)
	}
}
