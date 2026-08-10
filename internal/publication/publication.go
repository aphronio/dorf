package publication

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/aphronio/dorf/internal/evidence"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

type GitHub interface {
	RemoteHead(context.Context, githubapi.Authority, string) (string, bool, error)
	PushToken(context.Context, githubapi.Authority) (string, error)
	PullRequests(context.Context, githubapi.Authority, string, string) ([]githubapi.PullRequest, error)
	CreatePullRequest(context.Context, githubapi.Authority, string, string, string, string) (githubapi.PullRequest, error)
	UpdatePullRequest(context.Context, githubapi.Authority, int64, string, string, string) (githubapi.PullRequest, error)
}

type Repository interface {
	Relation(context.Context, spine.Job, string) (string, error)
	Push(context.Context, spine.Job, string) error
}

type Service struct {
	Store      postgres.Store
	GitHub     GitHub
	Repository Repository
	Evidence   evidence.Store
	Barrier    any
	// ClaimCheck validates the current durable executor claim immediately
	// before a locally-recorded receipt makes an external effect durable.
	ClaimCheck func(context.Context) error
}

type AttentionError struct{ Reason string }

func (e *AttentionError) Error() string { return e.Reason }

// Push reconciles and, when necessary, pushes the Job's exact Revision. It
// owns no scheduling: its caller decides when this independently recoverable
// effect is eligible to run.
func (s Service) Push(ctx context.Context, jobID, revision string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error { return s.pushFenced(ctx, jobID, revision) })
}

func (s Service) pushFenced(ctx context.Context, jobID, revision string) error {
	job, err := s.Store.Job(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Revision != revision {
		return fmt.Errorf("publication scope Revision=%s conflicts with Job Revision=%s", revision, job.Revision)
	}
	if !job.AdmissionOpen || job.CleanupState != spine.CleanupPending {
		return fmt.Errorf("publication cannot mutate Git or GitHub after cleanup begins")
	}
	if job.WorkflowPhase != "publishing" {
		return fmt.Errorf("Job %s publication is not active (phase %s)", job.ID, job.WorkflowPhase)
	}
	pushAction, _, err := s.Store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
	if _, present, err := s.GitHub.RemoteHead(ctx, authority, job.BaseBranch); err != nil {
		_ = s.Store.UncertainAction(ctx, pushAction.ID)
		return err
	} else if !present {
		return s.block(ctx, job, fmt.Sprintf("explicit GitHub base branch %s does not resolve in %s", job.BaseBranch, job.GitHubRepository))
	}
	remote, present, err := s.GitHub.RemoteHead(ctx, authority, job.Branch)
	if err != nil {
		_ = s.Store.UncertainAction(ctx, pushAction.ID)
		return err
	}
	relation := ""
	if present && remote != job.Revision {
		var relationErr error
		relation, relationErr = s.Repository.Relation(ctx, job, remote)
		if relationErr != nil {
			return s.block(ctx, job, relationErr.Error())
		}
	}
	pushDecision, decisionErr := planPush(present, remote, job.Revision, relation)
	if decisionErr != nil {
		return s.block(ctx, job, decisionErr.Error())
	}
	if pushDecision == "push" {
		token, err := s.GitHub.PushToken(ctx, authority)
		if err != nil {
			_ = s.Store.UncertainAction(ctx, pushAction.ID)
			return err
		}
		if err := s.Repository.Push(ctx, job, token); err != nil {
			_ = s.Store.UncertainAction(ctx, pushAction.ID)
			return err
		}
		if err := s.reach(ctx, spine.BarrierPushAccepted, job.ID, pushAction.ID); err != nil {
			return err
		}
	}
	if pushAction.State != spine.ActionSucceeded {
		if err := s.recordAfterClaim(ctx, pushAction.ID, func() error {
			return s.Store.RecordPush(ctx, pushAction.ID, job.Revision)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Propose reconciles and, when necessary, creates or refreshes the one exact
// GitHub pull-request proposal for the Job Revision. Push must already have a
// durable success receipt; PostgreSQL also enforces that ordering.
func (s Service) Propose(ctx context.Context, jobID, revision string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error { return s.proposeFenced(ctx, jobID, revision) })
}

func (s Service) proposeFenced(ctx context.Context, jobID, revision string) error {
	job, err := s.Store.Job(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Revision != revision {
		return fmt.Errorf("publication scope Revision=%s conflicts with Job Revision=%s", revision, job.Revision)
	}
	if !job.AdmissionOpen || job.CleanupState != spine.CleanupPending {
		return fmt.Errorf("publication cannot mutate GitHub after cleanup begins")
	}
	if job.WorkflowPhase == "published" {
		proposal, err := s.Store.Proposal(ctx, job.ID)
		if err == nil && proposal != nil && !proposal.Stale && proposal.BodyDigest != "" {
			return nil
		}
	}
	if job.WorkflowPhase != "publishing" {
		return fmt.Errorf("Job %s publication is not active (phase %s)", job.ID, job.WorkflowPhase)
	}
	declared, err := s.Store.DeclaredChecks(ctx, job.ID)
	if err != nil {
		return err
	}
	checks, err := s.Store.Checks(ctx, job.ID)
	if err != nil {
		return err
	}
	records, err := s.Store.Evidence(ctx, job.ID)
	if err != nil {
		return err
	}
	plans, err := s.Store.ReviewPlans(ctx, job.ID)
	if err != nil {
		return err
	}
	runs, err := s.Store.AllReviewRuns(ctx, job.ID)
	if err != nil {
		return err
	}
	var plan *spine.ReviewPlanRecord
	for i := range plans {
		if plans[i].Revision == job.Revision {
			plan = &plans[i]
		}
	}
	assessment := spine.AssessReviewReadiness(job, declared, checks, records, s.Evidence, plan, runs)
	if !assessment.Ready || assessment.Revision != job.Revision {
		return s.block(ctx, job, "publication lost exact-Revision readiness: "+assessment.Reason)
	}
	body := Body(job, assessment, checks, records, runs)
	bodyDigest := BodyDigest(body)
	title := Title(job.Goal)
	_, pullAction, err := s.Store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	owner := strings.SplitN(job.GitHubRepository, "/", 2)[0]
	authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
	pulls, err := s.GitHub.PullRequests(ctx, authority, owner, job.Branch)
	if err != nil {
		_ = s.Store.UncertainAction(ctx, pullAction.ID)
		return err
	}
	if len(pulls) > 0 {
		if err := s.Store.UncertainAction(ctx, pullAction.ID); err != nil {
			return err
		}
	}
	stored, err := s.Store.Proposal(ctx, job.ID)
	if err != nil {
		return err
	}
	pullDecision, pull, err := planPull(job, pulls, stored, title, body)
	if err != nil {
		return s.block(ctx, job, err.Error())
	}
	mutated := false
	validationProposal := stored
	switch pullDecision {
	case "create":
		if err := s.Store.UncertainAction(ctx, pullAction.ID); err != nil {
			return err
		}
		pull, err = s.GitHub.CreatePullRequest(ctx, authority, title, body, job.Branch, job.BaseBranch)
		mutated = err == nil
	case "update":
		if validationProposal == nil {
			validationProposal = &spine.GitHubProposal{Number: pull.Number}
		}
		pull, err = s.GitHub.UpdatePullRequest(ctx, authority, pull.Number, title, body, job.BaseBranch)
		mutated = err == nil
	case "adopt":
	}
	if err != nil {
		return err
	}
	if err := validatePull(job, pull, validationProposal, title); err != nil {
		return s.block(ctx, job, err.Error())
	}
	if pull.Body != body {
		return s.block(ctx, job, "GitHub pull-request response did not retain the exact projected body")
	}
	if mutated {
		if err := s.reach(ctx, spine.BarrierPullRequestAccepted, job.ID, pullAction.ID); err != nil {
			return err
		}
	}
	proposal := spine.GitHubProposal{JobID: job.ID, Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation, BaseBranch: job.BaseBranch, HeadBranch: job.Branch, Number: pull.Number, URL: pull.URL, ProposedRevision: job.Revision, ObservedRemoteHead: pull.HeadSHA, BodyDigest: bodyDigest}
	return s.recordAfterClaim(ctx, pullAction.ID, func() error {
		return s.Store.RecordProposal(ctx, pullAction.ID, proposal)
	})
}

func planPush(present bool, remote, revision, relation string) (string, error) {
	if !present {
		return "push", nil
	}
	if remote == revision {
		return "adopt", nil
	}
	if relation == "behind" {
		return "push", nil
	}
	return "", &AttentionError{Reason: fmt.Sprintf("remote head %s is %s relative to exact Revision %s; refusing force or rewrite", remote, relation, revision)}
}

func planPull(job spine.Job, pulls []githubapi.PullRequest, stored *spine.GitHubProposal, title, body string) (string, githubapi.PullRequest, error) {
	if len(pulls) > 1 {
		return "", githubapi.PullRequest{}, &AttentionError{Reason: fmt.Sprintf("GitHub has %d pull requests across states or bases for the exact Job head; refusing duplicate publication", len(pulls))}
	}
	if len(pulls) == 0 {
		if stored != nil {
			return "", githubapi.PullRequest{}, &AttentionError{Reason: fmt.Sprintf("recorded pull request #%d is no longer discoverable for the exact Job head", stored.Number)}
		}
		return "create", githubapi.PullRequest{}, nil
	}
	pull := pulls[0]
	if err := validatePullIdentity(job, pull, stored); err != nil {
		return "", githubapi.PullRequest{}, err
	}
	if pull.Title != title || pull.Body != body {
		return "update", pull, nil
	}
	return "adopt", pull, nil
}

func (s Service) block(ctx context.Context, job spine.Job, reason string) error {
	if err := s.Store.BlockPublication(ctx, job.ID, job.Revision, reason); err != nil {
		return err
	}
	return &AttentionError{Reason: reason}
}

func (s Service) reach(ctx context.Context, point, jobID, identity string) error {
	barrier, ok := s.Barrier.(interface {
		ReachWorkflow(context.Context, string, string, string) error
	})
	if !ok {
		return nil
	}
	return barrier.ReachWorkflow(ctx, point, jobID, identity)
}

func (s Service) requireClaim(ctx context.Context) error {
	if s.ClaimCheck == nil {
		return nil
	}
	return s.ClaimCheck(ctx)
}

func (s Service) recordAfterClaim(ctx context.Context, actionID string, record func() error) error {
	return claimBeforeRecord(ctx, s.requireClaim, func() error {
		return s.Store.UncertainAction(ctx, actionID)
	}, record)
}

func claimBeforeRecord(ctx context.Context, claimCheck func(context.Context) error, uncertain func() error, record func() error) error {
	if err := claimCheck(ctx); err != nil {
		_ = uncertain()
		return err
	}
	return record()
}

func validatePullIdentity(job spine.Job, pull githubapi.PullRequest, stored *spine.GitHubProposal) error {
	if pull.Repository != job.GitHubRepository || pull.Head != job.Branch || pull.Base != job.BaseBranch || pull.Number < 1 || pull.URL == "" {
		return &AttentionError{Reason: "GitHub pull request conflicts with exact repository + head + base identity"}
	}
	if pull.State != "open" {
		return &AttentionError{Reason: fmt.Sprintf("GitHub pull request #%d for the exact Job head is %s; refusing to create a replacement", pull.Number, pull.State)}
	}
	if pull.Draft {
		return &AttentionError{Reason: fmt.Sprintf("GitHub pull request #%d is draft; direct REST publication cannot prove ready state", pull.Number)}
	}
	if pull.HeadSHA != job.Revision {
		return &AttentionError{Reason: fmt.Sprintf("GitHub pull request head Revision %s conflicts with exact proposed Revision %s", pull.HeadSHA, job.Revision)}
	}
	if stored != nil && stored.Number != pull.Number {
		return &AttentionError{Reason: fmt.Sprintf("recorded pull request #%d conflicts with discovered pull request #%d", stored.Number, pull.Number)}
	}
	return nil
}

func validatePull(job spine.Job, pull githubapi.PullRequest, stored *spine.GitHubProposal, title string) error {
	if err := validatePullIdentity(job, pull, stored); err != nil {
		return err
	}
	if pull.Title != title {
		return &AttentionError{Reason: "GitHub pull-request response did not retain the exact projected title"}
	}
	return nil
}

func Title(goal string) string {
	line := strings.TrimSpace(strings.SplitN(goal, "\n", 2)[0])
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		line = "Dorf coding Job"
	}
	if len(line) > 120 {
		line = strings.TrimSpace(truncateUTF8(line, 120))
	}
	return line
}

func BodyDigest(body string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(body))) }

func Body(job spine.Job, readiness spine.ReadinessAssessment, checks []spine.Check, evidence []spine.Evidence, runs []spine.ReviewRunView) string {
	digests := make(map[string]string, len(evidence))
	for _, item := range evidence {
		digests[item.ID] = item.Digest
	}
	var lines []string
	lines = append(lines, "## Goal", "", projectGoal(job.Goal), "", "## Exact proposal", "", "- Base: `"+job.BaseBranch+"`", "- Head: `"+job.Branch+"`", "- Revision: `"+job.Revision+"`", "", "## Checks", "")
	currentChecks := make([]spine.Check, 0)
	for _, check := range checks {
		if check.Revision == job.Revision {
			currentChecks = append(currentChecks, check)
		}
	}
	sort.Slice(currentChecks, func(i, j int) bool { return currentChecks[i].Name < currentChecks[j].Name })
	for _, check := range currentChecks {
		lines = append(lines, fmt.Sprintf("- %s: %s (Evidence `%s`, sha256 `%s`)", check.Name, check.State, check.EvidenceID, digests[check.EvidenceID]))
	}
	lines = append(lines, "", "## Selected review", "")
	runsByID := make(map[string]spine.ReviewRunView, len(runs))
	for _, run := range runs {
		runsByID[run.ID] = run
	}
	for _, verification := range readiness.ReviewEvidence {
		run, ok := runsByID[verification.AgentRunID]
		if !ok || run.Revision != job.Revision {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: AgentRun `%s` completed with observed Evidence `%s`, sha256 `%s`; feedback Message `%s` handled by an implementation AgentRun", run.Role, run.ID, verification.ObservedEvidenceID, digests[verification.ObservedEvidenceID], run.FeedbackMessageID))
	}
	if len(readiness.ReviewEvidence) == 0 {
		lines = append(lines, "- ReviewPolicy selected no agent review.")
	}
	attention := "none"
	if job.WorkflowAttention != "" {
		attention = job.WorkflowAttention
	}
	lines = append(lines, "", "## Readiness", "", "- Status: "+readiness.Status, "- Reason: "+readiness.Reason, "- Remaining attention: "+attention, "- Inspect: `dorf inspect "+job.ID+"`")
	return strings.Join(lines, "\n") + "\n"
}

func projectGoal(goal string) string {
	goal = strings.TrimSpace(goal)
	const limit = 1200
	if len(goal) <= limit {
		return goal
	}
	return strings.TrimSpace(truncateUTF8(goal, limit)) + "\n\n[Goal projection truncated; inspect the Job for the complete admitted goal.]"
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

type GitRepository struct {
	Sandbox   incus.Sandbox
	Workspace string
	Run       func(context.Context, []string, []string) ([]byte, []byte, error)
}

func (r GitRepository) Relation(ctx context.Context, job spine.Job, remote string) (string, error) {
	if !postgres.ValidRevision(remote) {
		return "", &AttentionError{Reason: "remote head is not an exact commit OID"}
	}
	name := r.Sandbox.Name(job.ID)
	present, err := r.Sandbox.Exec(ctx, name, nil, "git", "-C", r.Workspace, "cat-file", "-e", remote+"^{commit}")
	if err != nil || present.ExitCode != 0 {
		return "", &AttentionError{Reason: fmt.Sprintf("remote head %s is not present in the admitted repository; cannot prove safe ancestry", remote)}
	}
	behind, err := r.Sandbox.Exec(ctx, name, nil, "git", "-C", r.Workspace, "merge-base", "--is-ancestor", remote, job.Revision)
	if err != nil {
		return "", err
	}
	if behind.ExitCode == 0 {
		return "behind", nil
	}
	if behind.ExitCode != 1 {
		return "", fmt.Errorf("classify remote head ancestry: git merge-base exited %d: %s", behind.ExitCode, boundedStderr(behind.Stderr))
	}
	ahead, err := r.Sandbox.Exec(ctx, name, nil, "git", "-C", r.Workspace, "merge-base", "--is-ancestor", job.Revision, remote)
	if err != nil {
		return "", err
	}
	if ahead.ExitCode == 0 {
		return "ahead", nil
	}
	if ahead.ExitCode != 1 {
		return "", fmt.Errorf("classify exact Revision ancestry: git merge-base exited %d: %s", ahead.ExitCode, boundedStderr(ahead.Stderr))
	}
	return "divergent", nil
}

func boundedStderr(stderr string) string {
	const limit = 512
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return "no stderr"
	}
	if len(detail) > limit {
		return truncateUTF8(detail, limit) + " [truncated]"
	}
	return detail
}

func (r GitRepository) Push(ctx context.Context, job spine.Job, token string) error {
	if token == "" {
		return fmt.Errorf("repository push requires one ephemeral GitHub App token")
	}
	name := r.Sandbox.Name(job.ID)
	verify, err := r.Sandbox.Exec(ctx, name, nil, "bash", "-c", `set -eu
workspace=$1; revision=$2; branch=$3; bundle=$4
test "$(git -C "$workspace" rev-parse "refs/heads/$branch")" = "$revision"
test "$(git -C "$workspace" rev-parse "$revision^{commit}")" = "$revision"
git -C "$workspace" bundle create "$bundle" "refs/heads/$branch"
cat "$bundle"
rm -f -- "$bundle"`, "dorf-publication-export", r.Workspace, job.Revision, job.Branch, "/tmp/dorf-publication-"+job.ID+".bundle")
	if err != nil {
		return err
	}
	if verify.ExitCode != 0 || verify.Stdout == "" {
		return fmt.Errorf("export exact Git objects for publication: %s", strings.TrimSpace(verify.Stderr))
	}
	temporary, err := os.MkdirTemp("", "dorf-push-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	bundle := filepath.Join(temporary, "objects.bundle")
	if err := os.WriteFile(bundle, []byte(verify.Stdout), 0o600); err != nil {
		return err
	}
	repository := filepath.Join(temporary, "repository.git")
	askpass := filepath.Join(temporary, "askpass")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' x-access-token;; *) printf '%s\\n' \"$DORF_EPHEMERAL_GITHUB_TOKEN\";; esac\n"), 0o700); err != nil {
		return err
	}
	run := r.Run
	if run == nil {
		run = runGit
	}
	isolatedGit := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if _, stderr, err := run(ctx, isolatedGit, []string{"git", "clone", "--bare", bundle, repository}); err != nil {
		return fmt.Errorf("materialize exact publication objects: %s", sanitize(stderr, token))
	}
	remoteURL := "https://github.com/" + job.GitHubRepository + ".git"
	env := append(isolatedGit, "GIT_ASKPASS="+askpass, "DORF_EPHEMERAL_GITHUB_TOKEN="+token)
	args := []string{"git", "-c", "credential.helper=", "-C", repository, "push", "--porcelain", remoteURL, job.Revision + ":refs/heads/" + job.Branch}
	if _, stderr, err := run(ctx, env, args); err != nil {
		return fmt.Errorf("push exact Revision to recorded head without force: %s", sanitize(stderr, token))
	}
	return nil
}

func runGit(ctx context.Context, additions, args []string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Env = append(os.Environ(), additions...)
	var stdout, stderr strings.Builder
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

func sanitize(contents []byte, token string) string {
	return strings.TrimSpace(strings.ReplaceAll(string(contents), token, "[REDACTED_GITHUB_TOKEN]"))
}
