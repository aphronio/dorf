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
		decision string
		roles    []Role
	}{
		{"docs only", []string{"README.md", "docs/review.md"}, "no-review", nil},
		{"docs web marker stays documentation", []string{"docs/web/guide.md"}, "no-review", nil},
		{"docs auth marker stays documentation", []string{"docs/auth/README.md"}, "no-review", nil},
		{"browser UI", []string{"web/app.tsx"}, "selected", []Role{RoleBrowserUI}},
		{"authentication authority", []string{"internal/auth/policy.go"}, "selected", []Role{RoleAuthAuthority}},
		{"docs markers plus real UI and auth", []string{"docs/web/guide.md", "docs/auth/README.md", "web/app.tsx", "internal/auth/policy.go"}, "selected", []Role{RoleAuthAuthority, RoleBrowserUI}},
		{"unknown", []string{"internal/core/service.go"}, "selected", []Role{RoleGeneral}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facts, err := FactsFromPaths(base, revision, test.paths)
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
