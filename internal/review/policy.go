package review

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type Role string

const (
	RoleBrowserUI        Role = "browser-ui"
	RoleAuthAuthority    Role = "auth-authority"
	RoleCriticalBoundary Role = "critical-boundary"
	RoleGeneral          Role = "general"
)

var allowedRoles = []Role{RoleAuthAuthority, RoleBrowserUI, RoleCriticalBoundary, RoleGeneral}

type ChangeFacts struct {
	Revision          string   `json:"revision"`
	BaseRevision      string   `json:"base_revision"`
	Paths             []string `json:"paths"`
	DocumentationOnly bool     `json:"documentation_only"`
	BrowserUI         bool     `json:"browser_ui"`
	Authentication    bool     `json:"authentication_authority"`
	Unknown           bool     `json:"unknown"`
}

type Reason struct {
	Role   Role   `json:"role"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

type ReviewPlan struct {
	Decision string   `json:"decision"`
	Roles    []Role   `json:"roles"`
	Reasons  []Reason `json:"reasons"`
}

// FactsFromPaths classifies only facts that are mechanically apparent from an
// immutable Git diff. Anything else remains unknown and is not guessed from
// agent prose.
func FactsFromPaths(baseRevision, revision string, paths []string) (ChangeFacts, error) {
	if strings.TrimSpace(baseRevision) == "" || strings.TrimSpace(revision) == "" {
		return ChangeFacts{}, fmt.Errorf("change facts require exact base and current Revisions")
	}
	if len(paths) == 0 {
		return ChangeFacts{}, fmt.Errorf("change facts require at least one changed path")
	}
	clean := make([]string, 0, len(paths))
	docsOnly, browserUI, authentication := true, false, false
	covered := true
	seen := map[string]bool{}
	for _, raw := range paths {
		path := filepath.ToSlash(strings.TrimSpace(raw))
		if path == "" || strings.HasPrefix(path, "/") || path == ".." || strings.HasPrefix(path, "../") {
			return ChangeFacts{}, fmt.Errorf("unsafe changed path %q", raw)
		}
		if seen[path] {
			continue
		}
		seen[path] = true
		clean = append(clean, path)
		doc := documentationPath(path)
		ui := !doc && browserUIPath(path)
		auth := !doc && authenticationPath(path)
		docsOnly = docsOnly && doc
		browserUI = browserUI || ui
		authentication = authentication || auth
		covered = covered && (doc || ui || auth)
	}
	sort.Strings(clean)
	return ChangeFacts{
		Revision: revision, BaseRevision: baseRevision, Paths: clean,
		DocumentationOnly: docsOnly, BrowserUI: browserUI, Authentication: authentication,
		Unknown: !docsOnly && !covered,
	}, nil
}

// ReviewPolicy is pure and deterministic. Explicit facts select the mandatory
// review floor; unknown classification selects one general read-only reviewer.
func ReviewPolicy(facts ChangeFacts) (ReviewPlan, error) {
	if facts.Revision == "" || facts.BaseRevision == "" || len(facts.Paths) == 0 {
		return ReviewPlan{}, fmt.Errorf("incomplete ChangeFacts cannot select review")
	}
	selected := map[Role]bool{}
	var reasons []Reason
	add := func(role Role, source, detail string) {
		if !selected[role] {
			selected[role] = true
			reasons = append(reasons, Reason{Role: role, Source: source, Detail: detail})
		}
	}
	if facts.BrowserUI {
		add(RoleBrowserUI, "mandatory", "browser/UI paths changed")
	}
	if facts.Authentication {
		add(RoleAuthAuthority, "mandatory", "authentication or authority paths changed")
	}
	if facts.Unknown {
		add(RoleGeneral, "unknown", "change risk could not be classified mechanically")
	}
	roles := make([]Role, 0, len(selected))
	for role := range selected {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
	sort.Slice(reasons, func(i, j int) bool {
		if reasons[i].Role == reasons[j].Role {
			return reasons[i].Source < reasons[j].Source
		}
		return reasons[i].Role < reasons[j].Role
	})
	if len(roles) == 0 {
		roles = nil
	}
	decision := "selected"
	if len(roles) == 0 {
		decision = "no-review"
	}
	return ReviewPlan{Decision: decision, Roles: roles, Reasons: reasons}, nil
}

func Allowed(role Role) bool {
	for _, allowed := range allowedRoles {
		if role == allowed {
			return true
		}
	}
	return false
}

func documentationPath(path string) bool {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(lower))
	return strings.HasPrefix(lower, "docs/") || strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, ".mdx") || base == "readme" || strings.HasPrefix(base, "readme.") || base == "license" || strings.HasPrefix(base, "license.")
}

func browserUIPath(path string) bool {
	lower := strings.ToLower(path)
	ext := strings.ToLower(filepath.Ext(lower))
	if ext == ".html" || ext == ".css" || ext == ".scss" || ext == ".sass" || ext == ".jsx" || ext == ".tsx" || ext == ".vue" || ext == ".svelte" {
		return true
	}
	return hasComponent(lower, "ui") || hasComponent(lower, "web") || hasComponent(lower, "frontend") || hasComponent(lower, "browser")
}

func authenticationPath(path string) bool {
	lower := strings.ToLower(path)
	for _, component := range []string{"auth", "authentication", "authorization", "authority", "permissions", "security"} {
		if hasComponent(lower, component) || strings.HasPrefix(strings.ToLower(filepath.Base(lower)), component+".") || strings.HasPrefix(strings.ToLower(filepath.Base(lower)), component+"_") {
			return true
		}
	}
	return false
}

func hasComponent(path, component string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == component {
			return true
		}
	}
	return false
}
