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
	RolePerformance      Role = "performance"
	RoleCriticalBoundary Role = "critical-boundary"
)

var allowedRoles = []Role{RoleAuthAuthority, RoleBrowserUI, RoleCriticalBoundary, RolePerformance}

type ChangeFacts struct {
	Revision            string   `json:"revision"`
	BaseRevision        string   `json:"base_revision"`
	Paths               []string `json:"paths"`
	ChecksGreen         bool     `json:"checks_green"`
	DocumentationOnly   bool     `json:"documentation_only"`
	BrowserUI           bool     `json:"browser_ui"`
	Authentication      bool     `json:"authentication_authority"`
	DeclaredPerformance bool     `json:"declared_performance"`
	Unknown             bool     `json:"unknown"`
}

type Reason struct {
	Role   Role   `json:"role"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

type ReviewPlan struct {
	Decision    string   `json:"decision"`
	Roles       []Role   `json:"roles"`
	Reasons     []Reason `json:"reasons"`
	NeedsTriage bool     `json:"needs_triage"`
}

// FactsFromPaths classifies only facts that are mechanically apparent from an
// immutable Git diff and a repository declaration. Anything else remains
// unknown and is not guessed from agent prose.
func FactsFromPaths(baseRevision, revision string, paths []string, checksGreen, declaredPerformance bool) (ChangeFacts, error) {
	if strings.TrimSpace(baseRevision) == "" || strings.TrimSpace(revision) == "" {
		return ChangeFacts{}, fmt.Errorf("change facts require exact base and current Revisions")
	}
	if !checksGreen {
		return ChangeFacts{}, fmt.Errorf("review policy requires green exact-Revision Checks")
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
		Revision: revision, BaseRevision: baseRevision, Paths: clean, ChecksGreen: true,
		DocumentationOnly: docsOnly, BrowserUI: browserUI, Authentication: authentication,
		DeclaredPerformance: declaredPerformance,
		Unknown:             !docsOnly && !covered,
	}, nil
}

// Plan is pure and deterministic. Requested roles can only add allowlisted
// review; they cannot remove the mandatory floor.
func ReviewPolicy(facts ChangeFacts, requested []Role) (ReviewPlan, error) {
	if !facts.ChecksGreen || facts.Revision == "" || facts.BaseRevision == "" || len(facts.Paths) == 0 {
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
	if facts.DeclaredPerformance {
		add(RolePerformance, "mandatory", "repository declared performance-sensitive change")
	}
	for _, role := range requested {
		if !Allowed(role) {
			return ReviewPlan{}, fmt.Errorf("implementation requested invalid or unsafe review Role %q", role)
		}
		add(role, "implementation-request", "implementation AgentRun requested additional review")
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
	if facts.Unknown {
		decision = "triage"
	} else if len(roles) == 0 {
		decision = "no-review"
	}
	return ReviewPlan{Decision: decision, Roles: roles, Reasons: reasons, NeedsTriage: facts.Unknown}, nil
}

func AddTriage(plan ReviewPlan, roles []Role, rationale string) (ReviewPlan, error) {
	if !plan.NeedsTriage || plan.Decision != "triage" {
		return ReviewPlan{}, fmt.Errorf("triage result is not admissible for policy decision %q", plan.Decision)
	}
	if strings.TrimSpace(rationale) == "" || len(rationale) > 4096 {
		return ReviewPlan{}, fmt.Errorf("triage requires bounded rationale")
	}
	selected := make(map[Role]bool, len(plan.Roles)+len(roles))
	for _, role := range plan.Roles {
		selected[role] = true
	}
	for _, role := range roles {
		if !Allowed(role) {
			return ReviewPlan{}, fmt.Errorf("triage attempted to add invalid or unsafe review Role %q", role)
		}
		if !selected[role] {
			selected[role] = true
			plan.Reasons = append(plan.Reasons, Reason{Role: role, Source: "review-triage", Detail: strings.TrimSpace(rationale)})
		}
	}
	plan.Roles = plan.Roles[:0]
	for role := range selected {
		plan.Roles = append(plan.Roles, role)
	}
	sort.Slice(plan.Roles, func(i, j int) bool { return plan.Roles[i] < plan.Roles[j] })
	sort.Slice(plan.Reasons, func(i, j int) bool {
		if plan.Reasons[i].Role == plan.Reasons[j].Role {
			return plan.Reasons[i].Source < plan.Reasons[j].Source
		}
		return plan.Reasons[i].Role < plan.Reasons[j].Role
	})
	plan.NeedsTriage = false
	if len(plan.Roles) == 0 {
		plan.Decision = "no-review"
	} else {
		plan.Decision = "selected"
	}
	return plan, nil
}

// TargetedReverification keeps the deterministic mandatory floor while
// replacing unknown-case broad triage with the exact Roles invalidated by one
// accepted material finding.
func TargetedReverification(plan ReviewPlan, affected []Role) (ReviewPlan, error) {
	if len(affected) == 0 {
		return ReviewPlan{}, fmt.Errorf("targeted re-verification requires at least one affected Role")
	}
	selected := map[Role]bool{}
	for _, role := range plan.Roles {
		selected[role] = true
	}
	for _, role := range affected {
		if !Allowed(role) {
			return ReviewPlan{}, fmt.Errorf("targeted re-verification contains invalid Role %q", role)
		}
		if !selected[role] {
			plan.Reasons = append(plan.Reasons, Reason{Role: role, Source: "accepted-finding", Detail: "accepted material finding invalidated this Role's claim"})
			selected[role] = true
		}
	}
	plan.Roles = plan.Roles[:0]
	for role := range selected {
		plan.Roles = append(plan.Roles, role)
	}
	sort.Slice(plan.Roles, func(i, j int) bool { return plan.Roles[i] < plan.Roles[j] })
	plan.Decision, plan.NeedsTriage = "selected", false
	return plan, nil
}

func Allowed(role Role) bool {
	for _, allowed := range allowedRoles {
		if role == allowed {
			return true
		}
	}
	return false
}

func AllowedRoles() []Role { return append([]Role(nil), allowedRoles...) }

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
