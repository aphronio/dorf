package publication

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/incus"
	incustest "github.com/aphronio/dorf/internal/incus/testkit"
	policy "github.com/aphronio/dorf/internal/review"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type sandboxRunner func(context.Context, string, []byte, ...string) (incus.Result, error)

func (f sandboxRunner) Run(ctx context.Context, command string, input []byte, args ...string) (incus.Result, error) {
	return f(ctx, command, input, args...)
}

func publicationSandbox(runner incustest.Runner) incus.Adapter {
	return incus.Adapter{Sandbox: incustest.Sandbox(runner, incus.Config{})}
}

func publicationOwner(_ context.Context, sandboxID string) (provider.Ownership, error) {
	return provider.Ownership{SandboxID: sandboxID}, nil
}

func TestPublicationIntentKeepsLaterAcceptedInputOutOfInFlightProof(t *testing.T) {
	cutoff := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	messages := []coding.MessageRecord{
		{Message: core.Message{ID: "before", AdmittedAt: cutoff.Add(-time.Second)}, ProducerID: "run-before"},
		{Message: core.Message{ID: "after", AdmittedAt: cutoff.Add(time.Second)}, ProducerID: "run-after"},
	}

	retained := coding.PublicationMessages(messages, cutoff)
	if len(retained) != 1 || retained[0].Message.ID != "before" || retained[0].ProducerID != "run-before" {
		t.Fatalf("retained Messages=%#v", retained)
	}
}

func TestGitRepositoryPushUsesRecordedOIDAndRefWithoutCredentialInArgvOrSandbox(t *testing.T) {
	const token = "installation-secret-token"
	var hostArgs, hostEnv [][]string
	repository := GitRepository{
		Workspace: "/workspace/job",
		Ownership: publicationOwner,
		Sandbox: publicationSandbox(sandboxRunner(func(_ context.Context, command string, input []byte, args ...string) (incus.Result, error) {
			joined := command + " " + strings.Join(args, " ") + string(input)
			if strings.Contains(joined, token) {
				t.Fatal("ephemeral token crossed the Incus execution seam")
			}
			return incus.Result{Stdout: "fake-bundle"}, nil
		})),
		Run: func(_ context.Context, env, args []string) ([]byte, []byte, error) {
			hostEnv = append(hostEnv, append([]string(nil), env...))
			hostArgs = append(hostArgs, append([]string(nil), args...))
			return nil, nil, nil
		},
	}
	revision := strings.Repeat("a", 40)
	job := coding.Job{Job: core.Job{ID: "job-exact"}, Revision: revision, Branch: "dorf/issue-43", GitHubRepository: "aphronio/dorf"}
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
			repository := GitRepository{Workspace: "/workspace/job", Ownership: publicationOwner, Sandbox: publicationSandbox(sandboxRunner(func(context.Context, string, []byte, ...string) (incus.Result, error) {
				result := incus.Result{ExitCode: exits[index]}
				index++
				return result, nil
			}))}
			job := coding.Job{Job: core.Job{ID: "job"}, Revision: strings.Repeat("b", 40)}
			got, err := repository.Relation(context.Background(), job, strings.Repeat("a", 40))
			if err != nil || got != name {
				t.Fatalf("relation=%s err=%v", got, err)
			}
		})
	}
	repository := GitRepository{Workspace: "/workspace/job", Ownership: publicationOwner, Sandbox: publicationSandbox(sandboxRunner(func(context.Context, string, []byte, ...string) (incus.Result, error) {
		return incus.Result{ExitCode: 1}, nil
	}))}
	if _, err := repository.Relation(context.Background(), coding.Job{Job: core.Job{ID: "job"}, Revision: strings.Repeat("b", 40)}, strings.Repeat("a", 40)); err == nil {
		t.Fatal("unknown remote object did not fail closed")
	}
}

func TestGitRepositoryRelationTreatsMergeBaseOperationalFailureAsError(t *testing.T) {
	calls := 0
	repository := GitRepository{Workspace: "/workspace/job", Ownership: publicationOwner, Sandbox: publicationSandbox(sandboxRunner(func(context.Context, string, []byte, ...string) (incus.Result, error) {
		calls++
		if calls == 1 {
			return incus.Result{ExitCode: 0}, nil
		}
		return incus.Result{ExitCode: 128, Stderr: strings.Repeat("fatal ancestry failure ", 80)}, nil
	}))}
	_, err := repository.Relation(context.Background(), coding.Job{Job: core.Job{ID: "job"}, Revision: strings.Repeat("b", 40)}, strings.Repeat("a", 40))
	if err == nil || !strings.Contains(err.Error(), "exited 128") || !strings.Contains(err.Error(), "[truncated]") || len(err.Error()) > 620 {
		t.Fatalf("merge-base operational error=%q", err)
	}
}

func TestBodyIsDeterministicExactRevisionProjection(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := coding.Job{Job: core.Job{ID: "job-1", Goal: "Implement durable publication"}, Revision: revision, Branch: "dorf/head", BaseBranch: "greenfield"}
	assessment := coding.ReadinessAssessment{Ready: true, Revision: revision, Reason: "exact revision and selected review settled"}
	evidence := []core.Evidence{{ID: "e-git", Digest: strings.Repeat("1", 64)}}
	first := Body(job, assessment, evidence, nil, nil)
	second := Body(job, assessment, evidence, nil, nil)
	if first != second || BodyDigest(first) != BodyDigest(second) || len(BodyDigest(first)) != 64 || !strings.Contains(first, revision) {
		t.Fatalf("non-deterministic or incomplete body:\n%s", first)
	}
}

func TestBodyProjectsOnlySelectedReviewEvidence(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := coding.Job{Job: core.Job{ID: "job-review", Goal: "Preserve opaque review feedback"}, Revision: revision, Branch: "dorf/review", BaseBranch: "main"}
	role := "general"
	requestID := coding.ReviewRequestMessageID(job.ID, revision, role)
	runID := core.AgentRunID(requestID)
	feedbackMessageID := core.MessageID(job.ID, core.MessageFromAgent, runID)
	observedID := core.EvidenceID(runID, "review-observation")
	runs := []coding.ReviewRunView{{
		ID: runID, JobID: job.ID, MessageID: requestID, Role: role, InputRevision: revision,
		Request: core.Message{ID: requestID, JobID: job.ID, FromKind: core.MessageFromWorkflow, FromID: coding.ReviewRequestFromID(revision, role), Input: "Review the exact Revision.", Intent: core.MessageFollow},
	}, {
		ID: "agent-run-unselected", JobID: job.ID, MessageID: "message-unselected-input", Role: "unselected", InputRevision: revision,
		Request: core.Message{ID: "message-unselected-input", JobID: job.ID, FromKind: core.MessageFromWorkflow, FromID: "unselected", Input: "Do not project this review.", Intent: core.MessageFollow},
	}}
	evidence := []core.Evidence{
		{ID: observedID, Digest: strings.Repeat("2", 64)},
		{ID: "e-unselected", Digest: strings.Repeat("3", 64)},
	}
	assessment := coding.ReadinessAssessment{Ready: true, Revision: revision, Reason: "review feedback handled"}
	plan := &coding.ReviewPlanRecord{JobID: job.ID, Revision: revision, Plan: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.Role(role)}}}

	body := Body(job, assessment, evidence, plan, runs)
	for _, want := range []string{role, runID, feedbackMessageID, observedID, strings.Repeat("2", 64)} {
		if !strings.Contains(body, want) {
			t.Fatalf("body is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "agent-run-unselected") || strings.Contains(body, "e-unselected") {
		t.Fatalf("body projected an unselected review:\n%s", body)
	}
}

func TestBodySeesOnlyExactRevisionReviewRuns(t *testing.T) {
	jobID := "job-exact-review"
	currentRevision := strings.Repeat("a", 40)
	oldRevision := strings.Repeat("f", 40)
	role := "general"
	currentRunID := core.AgentRunID(coding.ReviewRequestMessageID(jobID, currentRevision, role))
	oldRunID := core.AgentRunID(coding.ReviewRequestMessageID(jobID, oldRevision, role))
	runs := []coding.ReviewRunView{
		{ID: currentRunID, JobID: jobID, Role: role, InputRevision: currentRevision, Outcome: "completed"},
		// The stale same-Role run deliberately follows the current run.
		{ID: oldRunID, JobID: jobID, Role: role, InputRevision: oldRevision, Outcome: "failed"},
	}

	observedID := core.EvidenceID(currentRunID, "review-observation")
	plan := &coding.ReviewPlanRecord{JobID: jobID, Revision: currentRevision, Plan: policy.ReviewPlan{Decision: "selected", Roles: []policy.Role{policy.Role(role)}}}
	body := Body(
		coding.Job{Job: core.Job{ID: jobID, Goal: "keep publication exact"}, Revision: currentRevision, Branch: "dorf/exact", BaseBranch: "main"},
		coding.ReadinessAssessment{Ready: true, Revision: currentRevision, Reason: "exact review settled"},
		[]core.Evidence{{ID: observedID, Digest: strings.Repeat("1", 64)}},
		plan,
		runs,
	)
	if !strings.Contains(body, currentRunID) || strings.Contains(body, oldRunID) || strings.Contains(body, oldRevision) {
		t.Fatalf("body mixed review Revisions:\n%s", body)
	}
}

func TestPublicationTextProjectionRemainsValidUTF8AtLimits(t *testing.T) {
	goal := strings.Repeat("a", 1199) + "界tail"
	if got := projectGoal(goal); !utf8.ValidString(got) || len(got) > 1300 {
		t.Fatalf("goal projection bytes=%d valid UTF-8=%v", len(got), utf8.ValidString(got))
	}

	title := Title(strings.Repeat("a", 119) + "界tail")
	if !utf8.ValidString(title) || len(title) > 120 {
		t.Fatalf("title=%q bytes=%d valid=%t", title, len(title), utf8.ValidString(title))
	}
}

func TestPullRequestExactIdentityAllowsOneAndRejectsConflicts(t *testing.T) {
	revision := strings.Repeat("a", 40)
	job := coding.Job{GitHubRepository: "aphronio/dorf", Branch: "dorf/head", BaseBranch: "greenfield", Revision: revision}
	exact := githubapi.PullRequest{Number: 43, URL: "https://github.com/aphronio/dorf/pull/43", Title: "title", State: "open", Repository: "aphronio/dorf", Head: "dorf/head", HeadSHA: revision, Base: "greenfield", Body: "body"}
	if err := validatePull(job, exact, nil, exact.Title); err != nil {
		t.Fatal(err)
	}
	stored := &coding.Proposal{Number: 43}
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
	job := coding.Job{GitHubRepository: "aphronio/dorf", Branch: "dorf/head", BaseBranch: "greenfield", Revision: strings.Repeat("a", 40)}
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
	stored := &coding.Proposal{Number: 43}
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
	job := coding.Job{GitHubRepository: "aphronio/dorf", Branch: "dorf/head", BaseBranch: "greenfield", Revision: strings.Repeat("a", 40)}
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

func TestSanitizeAlwaysRedactsCredential(t *testing.T) {
	if got := sanitize([]byte("failure secret-value suffix"), "secret-value"); got != "failure [REDACTED_GITHUB_TOKEN] suffix" {
		t.Fatal(got)
	}
}

func TestPublicationRefusesDurableRecordWithoutClaimCheck(t *testing.T) {
	recorded := false
	err := (Service{}).recordAfterClaim(context.Background(), func() error {
		recorded = true
		return nil
	})
	if err == nil || recorded {
		t.Fatalf("missing durable executor claim check recorded=%t err=%v", recorded, err)
	}
}

func TestPublicationLostClaimDoesNotRecordAttention(t *testing.T) {
	claimLost := errors.New("claim lost")
	service := (Service{}).WithClaimCheck(func(context.Context) error { return claimLost })
	err := service.block(context.Background(), coding.Job{Job: core.Job{ID: "job-1"}, Revision: "revision-1"}, core.Action{ID: "action-1"}, "stale attention")
	if !errors.Is(err, claimLost) {
		t.Fatalf("block error = %v", err)
	}
}
