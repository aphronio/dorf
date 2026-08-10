package publication

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
)

type sandboxRunner func(context.Context, string, []byte, ...string) (incus.Result, error)

func (f sandboxRunner) Run(ctx context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	return f(ctx, command, input, args...)
}

func TestClaimLossMarksActionUncertainWithoutRecordingReceipt(t *testing.T) {
	claimLost := errors.New("claim lost")
	recorded, uncertain := false, false
	err := claimBeforeRecord(context.Background(), func(context.Context) error { return claimLost }, func() error {
		uncertain = true
		return nil
	}, func() error {
		recorded = true
		return nil
	})
	if !errors.Is(err, claimLost) || recorded || !uncertain {
		t.Fatalf("claim-loss receipt recorded=%v uncertain=%v err=%v", recorded, uncertain, err)
	}
}

func TestGitRepositoryPushUsesRecordedOIDAndRefWithoutCredentialInArgvOrSandbox(t *testing.T) {
	const token = "installation-secret-token"
	var hostArgs, hostEnv [][]string
	repository := GitRepository{
		Workspace: "/workspace/job",
		Sandbox: incus.Sandbox{Runner: sandboxRunner(func(_ context.Context, command string, input []byte, args ...string) (incus.Result, error) {
			joined := command + " " + strings.Join(args, " ") + string(input)
			if strings.Contains(joined, token) {
				t.Fatal("ephemeral token crossed the Incus execution seam")
			}
			return incus.Result{Stdout: "fake-bundle"}, nil
		})},
		Run: func(_ context.Context, env, args []string) ([]byte, []byte, error) {
			hostEnv = append(hostEnv, append([]string(nil), env...))
			hostArgs = append(hostArgs, append([]string(nil), args...))
			return nil, nil, nil
		},
	}
	revision := strings.Repeat("a", 40)
	job := spine.Job{ID: "job-exact", Revision: revision, Branch: "dorf/issue-43", GitHubRepository: "aphronio/dorf"}
	if err := repository.Push(context.Background(), job, token); err != nil {
		t.Fatal(err)
	}
	if len(hostArgs) != 2 {
		t.Fatalf("host commands=%v", hostArgs)
	}
	push := strings.Join(hostArgs[1], " ")
	if !strings.Contains(push, "credential.helper=") || !strings.Contains(push, revision+":refs/heads/dorf/issue-43") || strings.Contains(push, "HEAD:") || strings.Contains(push, "--force") || strings.Contains(push, token) {
		t.Fatalf("unsafe push argv: %s", push)
	}
	if strings.Contains(strings.Join(hostArgs[0], " "), token) {
		t.Fatal("credential appeared in materialization argv")
	}
	if !strings.Contains(strings.Join(hostEnv[1], " "), "DORF_EPHEMERAL_GITHUB_TOKEN="+token) {
		t.Fatal("host-only ephemeral credential was not supplied to askpass")
	}
}

func TestGitRepositoryRelationAllowsOnlyBehindAndClassifiesAheadDivergent(t *testing.T) {
	for name, exits := range map[string][]int{"behind": {0, 0}, "ahead": {0, 1, 0}, "divergent": {0, 1, 1}} {
		t.Run(name, func(t *testing.T) {
			index := 0
			repository := GitRepository{Workspace: "/workspace/job", Sandbox: incus.Sandbox{Runner: sandboxRunner(func(context.Context, string, []byte, ...string) (incus.Result, error) {
				result := incus.Result{ExitCode: exits[index]}
				index++
				return result, nil
			})}}
			job := spine.Job{ID: "job", Revision: strings.Repeat("b", 40)}
			got, err := repository.Relation(context.Background(), job, strings.Repeat("a", 40))
			if err != nil || got != name {
				t.Fatalf("relation=%s err=%v", got, err)
			}
		})
	}
	repository := GitRepository{Workspace: "/workspace/job", Sandbox: incus.Sandbox{Runner: sandboxRunner(func(context.Context, string, []byte, ...string) (incus.Result, error) {
		return incus.Result{ExitCode: 1}, nil
	})}}
	if _, err := repository.Relation(context.Background(), spine.Job{ID: "job", Revision: strings.Repeat("b", 40)}, strings.Repeat("a", 40)); err == nil {
		t.Fatal("unknown remote object did not fail closed")
	}
}

func TestGitRepositoryRelationTreatsMergeBaseOperationalFailureAsError(t *testing.T) {
	calls := 0
	repository := GitRepository{Workspace: "/workspace/job", Sandbox: incus.Sandbox{Runner: sandboxRunner(func(context.Context, string, []byte, ...string) (incus.Result, error) {
		calls++
		if calls == 1 {
			return incus.Result{ExitCode: 0}, nil
		}
		return incus.Result{ExitCode: 128, Stderr: strings.Repeat("fatal ancestry failure ", 80)}, nil
	})}}
	_, err := repository.Relation(context.Background(), spine.Job{ID: "job", Revision: strings.Repeat("b", 40)}, strings.Repeat("a", 40))
	if err == nil || !strings.Contains(err.Error(), "exited 128") || !strings.Contains(err.Error(), "[truncated]") || len(err.Error()) > 620 {
		t.Fatalf("merge-base operational error=%q", err)
	}
}

func TestBodyIsExactDeterministicRevisionProjectionWithoutNarration(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := spine.Job{ID: "job-1", Goal: "Implement durable publication", Revision: revision, Branch: "dorf/head", BaseBranch: "greenfield"}
	assessment := spine.ReadinessAssessment{Status: "ready", Ready: true, Revision: revision, Reason: "exact checks and selected review settled"}
	checks := []spine.Check{{Name: "smoke", State: "passed", Revision: revision, EvidenceID: "e-smoke"}, {Name: "check", State: "passed", Revision: revision, EvidenceID: "e-check"}}
	evidence := []spine.Evidence{{ID: "e-check", Digest: strings.Repeat("1", 64)}, {ID: "e-smoke", Digest: strings.Repeat("2", 64)}}
	first := Body(job, assessment, checks, evidence, nil)
	second := Body(job, assessment, checks, evidence, nil)
	if first != second || BodyDigest(first) != BodyDigest(second) || len(BodyDigest(first)) != 64 || !strings.Contains(first, revision) || !strings.Contains(first, "e-check") || strings.Index(first, "- check:") > strings.Index(first, "- smoke:") {
		t.Fatalf("non-deterministic or incomplete body:\n%s", first)
	}
	for _, forbidden := range []string{"transcript", "timeline", "token", "cost", "agent narration"} {
		if strings.Contains(strings.ToLower(first), forbidden) {
			t.Fatalf("body contains forbidden %q", forbidden)
		}
	}
}

func TestBodyProjectsReviewFeedbackAsMessageWithoutClassifyingIt(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := spine.Job{ID: "job-review", Goal: "Preserve opaque review feedback", Revision: revision, Branch: "dorf/review", BaseBranch: "main"}
	role := "critical-boundary"
	runID := spine.ReviewAgentRunID(job.ID, revision, role)
	requestID := spine.ReviewRequestMessageID(job.ID, revision, role)
	observedID := spine.EvidenceID(runID, "review-observation")
	runs := []spine.ReviewRunView{{
		AgentRun: spine.AgentRun{
			ID:        runID,
			MessageID: requestID,
			Role:      role,
			Revision:  revision,
		},
		Request:           spine.Message{ID: requestID, JobID: job.ID, FromKind: spine.MessageFromWorkflow, FromID: spine.ReviewRequestFromID(revision, role), Input: "Review the exact Revision.", Intent: spine.MessageFollow},
		FeedbackMessageID: "message-review-feedback",
	}, {
		AgentRun:          spine.AgentRun{ID: "agent-run-unselected", MessageID: "message-unselected-input", Role: "unselected", Revision: revision},
		Request:           spine.Message{ID: "message-unselected-input", JobID: job.ID, FromKind: spine.MessageFromWorkflow, FromID: "unselected", Input: "Do not project this review.", Intent: spine.MessageFollow},
		FeedbackMessageID: "message-unselected",
	}}
	evidence := []spine.Evidence{
		{ID: observedID, Digest: strings.Repeat("2", 64)},
		{ID: "e-unselected", Digest: strings.Repeat("3", 64)},
	}
	assessment := spine.ReadinessAssessment{
		Status: "ready",
		Reason: "review feedback handled",
		ReviewEvidence: []spine.ReviewEvidenceVerification{{
			AgentRunID: runID, Role: role, Revision: revision,
			ObservedEvidenceID: observedID, Verified: true,
		}},
	}

	body := Body(job, assessment, nil, evidence, runs)
	for _, want := range []string{role, "AgentRun `" + runID + "`", "feedback Message `message-review-feedback`", "handled by an implementation AgentRun", observedID, strings.Repeat("2", 64)} {
		if !strings.Contains(body, want) {
			t.Fatalf("body is missing %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"claim", "e-claim", "unselected", "material finding", "no material finding", "adjudication"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("body classified opaque feedback as %q:\n%s", forbidden, body)
		}
	}
}

func TestGoalProjectionPreservesExactUTF8AcrossJSONAndPullReconciliation(t *testing.T) {
	const truncated = "\n\n[Goal projection truncated; inspect the Job for the complete admitted goal.]"
	tests := []struct {
		name, goal, projected string
	}{
		{"multibyte rune crosses byte limit", strings.Repeat("a", 1199) + "界tail", strings.Repeat("a", 1199) + truncated},
		{"ASCII exactly at byte limit", strings.Repeat("a", 1200), strings.Repeat("a", 1200)},
		{"short Unicode", "修复精确发布边界 🙂", "修复精确发布边界 🙂"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := projectGoal(test.goal); got != test.projected || !utf8.ValidString(got) {
				t.Fatalf("projected valid UTF-8=%v\ngot=%q\nwant=%q", utf8.ValidString(got), got, test.projected)
			}
			revision := strings.Repeat("a", 40)
			job := spine.Job{ID: "job-unicode", Goal: test.goal, GitHubRepository: "aphronio/dorf", Revision: revision, Branch: "dorf/head", BaseBranch: "greenfield"}
			readiness := spine.ReadinessAssessment{Status: "ready", Ready: true, Revision: revision, Reason: "exact proof"}
			body := Body(job, readiness, nil, nil, nil)
			digest := BodyDigest(body)
			if !utf8.ValidString(body) || digest != BodyDigest(body) {
				t.Fatalf("body valid=%v digest=%s", utf8.ValidString(body), digest)
			}
			encoded, err := json.Marshal(struct {
				Body string `json:"body"`
			}{Body: body})
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Body != body || BodyDigest(decoded.Body) != digest {
				t.Fatal("JSON request text or digest differs from the local deterministic body")
			}
			title := Title(job.Goal)
			pull := githubapi.PullRequest{Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", Title: title, State: "open", Repository: job.GitHubRepository, Head: job.Branch, HeadSHA: revision, Base: job.BaseBranch, Body: decoded.Body}
			if decision, _, err := planPull(job, nil, nil, title, body); err != nil || decision != "create" {
				t.Fatalf("initial decision=%s err=%v", decision, err)
			}
			if decision, _, err := planPull(job, []githubapi.PullRequest{pull}, nil, title, body); err != nil || decision != "adopt" {
				t.Fatalf("create reconciliation=%s err=%v", decision, err)
			}
			pull.Body = "stale body"
			stored := &spine.GitHubProposal{Number: pull.Number}
			if decision, _, err := planPull(job, []githubapi.PullRequest{pull}, stored, title, body); err != nil || decision != "update" {
				t.Fatalf("refresh decision=%s err=%v", decision, err)
			}
			pull.Body = decoded.Body
			if decision, _, err := planPull(job, []githubapi.PullRequest{pull}, stored, title, body); err != nil || decision != "adopt" {
				t.Fatalf("update reconciliation=%s err=%v", decision, err)
			}
		})
	}
}

func TestPullRequestExactIdentityAllowsOneAndRejectsConflicts(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := spine.Job{GitHubRepository: "aphronio/dorf", Branch: "dorf/head", BaseBranch: "greenfield", Revision: revision}
	exact := githubapi.PullRequest{Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", Title: "title", State: "open", Repository: "aphronio/dorf", Head: "dorf/head", HeadSHA: revision, Base: "greenfield", Body: "body"}
	if err := validatePull(job, exact, nil, exact.Title); err != nil {
		t.Fatal(err)
	}
	stored := &spine.GitHubProposal{Number: 43}
	if err := validatePull(job, exact, stored, exact.Title); err != nil {
		t.Fatal(err)
	}
	repositoryConflict := exact
	repositoryConflict.Repository = "other/repo"
	headConflict := exact
	headConflict.Head = "other"
	shaConflict := exact
	shaConflict.HeadSHA = strings.Repeat("b", 40)
	baseConflict := exact
	baseConflict.Base = "main"
	for name, conflict := range map[string]githubapi.PullRequest{
		"repository": repositoryConflict,
		"head":       headConflict,
		"head SHA":   shaConflict,
		"base":       baseConflict,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePull(job, conflict, stored, exact.Title); err == nil {
				t.Fatal("identity conflict was accepted")
			}
		})
	}
	other := *stored
	other.Number = 44
	if err := validatePull(job, exact, &other, exact.Title); err == nil {
		t.Fatal("different recorded PR identity was adopted")
	}
}

func TestPushRecoveryAdoptsEqualPushesMissingOrBehindAndBlocksUnsafeHistory(t *testing.T) {
	revision, remote := strings.Repeat("b", 40), strings.Repeat("a", 40)
	for _, test := range []struct {
		name               string
		present            bool
		observed, relation string
		want               string
	}{{"remote equal after lost receipt", true, revision, "", "adopt"}, {"missing", false, "", "", "push"}, {"safely behind", true, remote, "behind", "push"}} {
		t.Run(test.name, func(t *testing.T) {
			got, err := planPush(test.present, test.observed, revision, test.relation)
			if err != nil || got != test.want {
				t.Fatalf("decision=%s err=%v", got, err)
			}
		})
	}
	for _, relation := range []string{"ahead", "divergent"} {
		if _, err := planPush(true, remote, revision, relation); err == nil || !strings.Contains(err.Error(), "refusing force or rewrite") {
			t.Fatalf("unsafe %s relation error=%v", relation, err)
		}
	}
}

func TestPullRecoveryPlansZeroCreateOneAdoptOrUpdateAndDuplicatesBlock(t *testing.T) {
	job := spine.Job{GitHubRepository: "aphronio/dorf", Branch: "dorf/head", BaseBranch: "greenfield", Revision: strings.Repeat("a", 40)}
	title := "exact title"
	exact := githubapi.PullRequest{Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", Title: title, State: "open", Repository: job.GitHubRepository, Head: job.Branch, HeadSHA: job.Revision, Base: job.BaseBranch, Body: "exact body"}
	if decision, _, err := planPull(job, nil, nil, title, exact.Body); err != nil || decision != "create" {
		t.Fatalf("zero decision=%s err=%v", decision, err)
	}
	if decision, _, err := planPull(job, []githubapi.PullRequest{exact}, nil, title, exact.Body); err != nil || decision != "adopt" {
		t.Fatalf("lost-create adoption=%s err=%v", decision, err)
	}
	stale := exact
	stale.Body = "old body"
	stored := &spine.GitHubProposal{Number: 43}
	if decision, pull, err := planPull(job, []githubapi.PullRequest{stale}, stored, title, exact.Body); err != nil || decision != "update" || pull.Number != 43 {
		t.Fatalf("same-PR refresh=%s pull=%#v err=%v", decision, pull, err)
	}
	if _, _, err := planPull(job, []githubapi.PullRequest{exact, exact}, stored, title, exact.Body); err == nil {
		t.Fatal("duplicate exact-identity PRs did not block")
	}
	if _, _, err := planPull(job, nil, stored, title, exact.Body); err == nil {
		t.Fatal("missing recorded PR was silently recreated")
	}
}

func TestPullRecoveryBlocksClosedWrongBaseAndDraftButRefreshesTitle(t *testing.T) {
	job := spine.Job{GitHubRepository: "aphronio/dorf", Branch: "dorf/head", BaseBranch: "greenfield", Revision: strings.Repeat("a", 40)}
	title := "exact title"
	exact := githubapi.PullRequest{Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", Title: title, State: "open", Repository: job.GitHubRepository, Head: job.Branch, HeadSHA: job.Revision, Base: job.BaseBranch, Body: "exact body"}
	for name, mutate := range map[string]func(*githubapi.PullRequest){
		"closed":     func(pull *githubapi.PullRequest) { pull.State = "closed" },
		"wrong base": func(pull *githubapi.PullRequest) { pull.Base = "main" },
		"draft":      func(pull *githubapi.PullRequest) { pull.Draft = true },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := exact
			mutate(&candidate)
			if decision, _, err := planPull(job, []githubapi.PullRequest{candidate}, nil, title, exact.Body); err == nil || decision != "" {
				t.Fatalf("conflicting candidate decision=%q err=%v", decision, err)
			}
		})
	}
	staleTitle := exact
	staleTitle.Title = "old title"
	if decision, pull, err := planPull(job, []githubapi.PullRequest{staleTitle}, nil, title, exact.Body); err != nil || decision != "update" || pull.Number != exact.Number {
		t.Fatalf("title refresh decision=%q pull=%#v err=%v", decision, pull, err)
	}
	if err := validatePull(job, exact, nil, title); err != nil {
		t.Fatalf("refreshed exact title was not accepted: %v", err)
	}
}

func TestTitlePreservesUTF8At120ByteBudget(t *testing.T) {
	for _, test := range []struct {
		name, goal, want string
	}{
		{"multibyte crosses boundary", strings.Repeat("a", 119) + "界tail", strings.Repeat("a", 119)},
		{"ASCII exact boundary", strings.Repeat("a", 120), strings.Repeat("a", 120)},
		{"short Unicode", "修复 publication 🙂", "修复 publication 🙂"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Title(test.goal)
			if got != test.want || !utf8.ValidString(got) || len(got) > 120 {
				t.Fatalf("title=%q bytes=%d valid=%t want=%q", got, len(got), utf8.ValidString(got), test.want)
			}
		})
	}
}

func TestServiceRetryAdoptsPreviouslyAcceptedExternalEffects(t *testing.T) {
	revision := strings.Repeat("a", 40)
	if decision, err := planPush(true, revision, revision, ""); err != nil || decision != "adopt" {
		t.Fatalf("accepted push decision=%s err=%v", decision, err)
	}
	job := spine.Job{GitHubRepository: "aphronio/dorf", Branch: "dorf/head", BaseBranch: "greenfield", Revision: revision}
	title := "exact title"
	pull := githubapi.PullRequest{Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", Title: title, State: "open", Repository: job.GitHubRepository, Head: job.Branch, HeadSHA: revision, Base: job.BaseBranch, Body: "exact body"}
	if decision, adopted, err := planPull(job, []githubapi.PullRequest{pull}, nil, title, pull.Body); err != nil || decision != "adopt" || adopted.Number != pull.Number {
		t.Fatalf("accepted pull request decision=%s pull=%#v err=%v", decision, adopted, err)
	}
}

func TestSanitizeAlwaysRedactsCredential(t *testing.T) {
	if got := sanitize([]byte("failure secret-value suffix"), "secret-value"); got != "failure [REDACTED_GITHUB_TOKEN] suffix" {
		t.Fatal(got)
	}
}
