package publication

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
	"github.com/aphronio/dorf/internal/postgres"
	policy "github.com/aphronio/dorf/internal/review"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type GitHub interface {
	RemoteHead(context.Context, githubapi.Authority, string) (string, bool, error)
	PushToken(context.Context, githubapi.Authority) (string, error)
	PullRequests(context.Context, githubapi.Authority, string, string) ([]githubapi.PullRequest, error)
	CreatePullRequest(context.Context, githubapi.Authority, string, string, string, string) (githubapi.PullRequest, error)
	UpdatePullRequest(context.Context, githubapi.Authority, int64, string, string, string) (githubapi.PullRequest, error)
}

type Repository interface {
	Relation(context.Context, coding.Job, string) (string, error)
	Push(context.Context, coding.Job, string) error
}

type WorkflowBarrier interface {
	ReachWorkflow(context.Context, string, string, string) error
}

type Service struct {
	Store      postgres.Store
	GitHub     GitHub
	Repository Repository
	Evidence   blob.Store
	Barrier    WorkflowBarrier
	claimCheck func(context.Context) error
}

// WithClaimCheck binds the durable executor claim used immediately before a
// locally-recorded Action success makes an external effect durable.
func (s Service) WithClaimCheck(check func(context.Context) error) Service {
	s.claimCheck = check
	return s
}

type AttentionError struct{ Reason string }

func (e *AttentionError) Error() string { return e.Reason }

type readinessView struct {
	Assessment coding.ReadinessAssessment
	Evidence   []core.Evidence
	Plan       *coding.ReviewPlanRecord
	ReviewRuns []coding.ReviewRunView
}

func (s Service) readiness(ctx context.Context, job coding.Job, intentAt time.Time) (readinessView, error) {
	var view readinessView
	var err error
	view.Evidence, err = s.Store.Evidence(ctx, job.ID)
	if err != nil {
		return view, err
	}
	plans, err := s.Store.ReviewPlans(ctx, job.ID)
	if err != nil {
		return view, err
	}
	messages, reviews, err := s.Store.CodingMessages(ctx, job.ID)
	if err != nil {
		return view, err
	}
	view.ReviewRuns = reviews
	messages = coding.PublicationMessages(messages, intentAt)
	var plan *coding.ReviewPlanRecord
	for i := range plans {
		if plans[i].Revision == job.Revision {
			plan = &plans[i]
		}
	}
	view.Plan = plan
	view.Assessment = coding.AssessReviewReadiness(job, view.Evidence, s.Evidence, plan, view.ReviewRuns, messages)
	return view, nil
}

// Push reconciles and, when necessary, pushes the Job's exact Revision. It
// owns no scheduling: its caller decides when this independently recoverable
// effect is eligible to run.
func (s Service) Push(ctx context.Context, jobID, revision string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error { return s.pushFenced(ctx, jobID, revision) })
}

func (s Service) pushFenced(ctx context.Context, jobID, revision string) error {
	job, err := s.Store.CodingJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Revision != revision {
		return fmt.Errorf("publication scope Revision=%s conflicts with Job Revision=%s", revision, job.Revision)
	}
	if !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		return fmt.Errorf("publication cannot mutate Git or GitHub after cleanup begins")
	}
	pushAction, _, err := s.Store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	readiness, err := s.readiness(ctx, job, pushAction.CreatedAt)
	if err != nil {
		return err
	}
	if !readiness.Assessment.Ready || readiness.Assessment.Revision != job.Revision {
		return s.block(ctx, job, pushAction, "publication lost exact-Revision readiness: "+readiness.Assessment.Reason)
	}
	authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
	if _, present, err := s.GitHub.RemoteHead(ctx, authority, job.BaseBranch); err != nil {
		return err
	} else if !present {
		return s.block(ctx, job, pushAction, fmt.Sprintf("explicit GitHub base branch %s does not resolve in %s", job.BaseBranch, job.GitHubRepository))
	}
	remote, present, err := s.GitHub.RemoteHead(ctx, authority, job.Branch)
	if err != nil {
		return err
	}
	relation := ""
	if present && remote != job.Revision {
		var relationErr error
		relation, relationErr = s.Repository.Relation(ctx, job, remote)
		if relationErr != nil {
			return s.block(ctx, job, pushAction, relationErr.Error())
		}
	}
	pushDecision, decisionErr := planPush(present, remote, job.Revision, relation)
	if decisionErr != nil {
		return s.block(ctx, job, pushAction, decisionErr.Error())
	}
	if pushDecision == "push" {
		token, err := s.GitHub.PushToken(ctx, authority)
		if err != nil {
			return err
		}
		if err := s.Repository.Push(ctx, job, token); err != nil {
			return err
		}
		if err := s.reach(ctx, core.BarrierPushAccepted, job.ID, pushAction.ID); err != nil {
			return err
		}
	}
	if pushAction.State != core.ActionSucceeded {
		if err := s.recordAfterClaim(ctx, func() error {
			return s.Store.RecordPush(ctx, pushAction.ID, job.Revision)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Propose reconciles and, when necessary, creates or refreshes the one exact
// GitHub pull-request proposal for the Job Revision. Push must already have a
// durable Action success; PostgreSQL also enforces that ordering.
func (s Service) Propose(ctx context.Context, jobID, revision string) error {
	return s.Store.WithJobFence(ctx, jobID, func() error { return s.proposeFenced(ctx, jobID, revision) })
}

func (s Service) proposeFenced(ctx context.Context, jobID, revision string) error {
	job, err := s.Store.CodingJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Revision != revision {
		return fmt.Errorf("publication scope Revision=%s conflicts with Job Revision=%s", revision, job.Revision)
	}
	if !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		return fmt.Errorf("publication cannot mutate GitHub after cleanup begins")
	}
	stored, err := s.Store.Proposal(ctx, job.ID)
	if err != nil {
		return err
	}
	if stored != nil && stored.ProposedRevision == job.Revision && stored.BodyDigest != "" {
		return nil
	}
	_, pullAction, err := s.Store.PublicationActions(ctx, job.ID, job.Revision)
	if err != nil {
		return err
	}
	readiness, err := s.readiness(ctx, job, pullAction.CreatedAt)
	if err != nil {
		return err
	}
	if !readiness.Assessment.Ready || readiness.Assessment.Revision != job.Revision {
		return s.block(ctx, job, pullAction, "publication lost exact-Revision readiness: "+readiness.Assessment.Reason)
	}
	body := Body(job, readiness.Assessment, readiness.Evidence, readiness.Plan, readiness.ReviewRuns)
	bodyDigest := BodyDigest(body)
	title := Title(job.Goal)
	owner := strings.SplitN(job.GitHubRepository, "/", 2)[0]
	authority := githubapi.Authority{Repository: job.GitHubRepository, InstallationID: job.GitHubInstallation}
	pulls, err := s.GitHub.PullRequests(ctx, authority, owner, job.Branch)
	if err != nil {
		return err
	}
	pullDecision, pull, err := planPull(job, pulls, stored, title, body)
	if err != nil {
		return s.block(ctx, job, pullAction, err.Error())
	}
	mutated := false
	validationProposal := stored
	switch pullDecision {
	case "create":
		pull, err = s.GitHub.CreatePullRequest(ctx, authority, title, body, job.Branch, job.BaseBranch)
		mutated = err == nil
	case "update":
		if validationProposal == nil {
			validationProposal = &coding.Proposal{Number: pull.Number}
		}
		pull, err = s.GitHub.UpdatePullRequest(ctx, authority, pull.Number, title, body, job.BaseBranch)
		mutated = err == nil
	case "adopt":
	}
	if err != nil {
		return err
	}
	if err := validatePull(job, pull, validationProposal, title); err != nil {
		return s.block(ctx, job, pullAction, err.Error())
	}
	if pull.Body != body {
		return s.block(ctx, job, pullAction, "GitHub pull-request response did not retain the exact projected body")
	}
	if mutated {
		if err := s.reach(ctx, core.BarrierPullRequestAccepted, job.ID, pullAction.ID); err != nil {
			return err
		}
	}
	proposal := coding.Proposal{JobID: job.ID, Number: pull.Number, URL: pull.URL, ProposedRevision: job.Revision, BodyDigest: bodyDigest}
	return s.recordAfterClaim(ctx, func() error {
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

func planPull(job coding.Job, pulls []githubapi.PullRequest, stored *coding.Proposal, title, body string) (string, githubapi.PullRequest, error) {
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

func (s Service) block(ctx context.Context, job coding.Job, action core.Action, reason string) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	if err := s.Store.BlockPublication(ctx, job.ID, job.Revision, action.ID, reason); err != nil {
		return err
	}
	return &AttentionError{Reason: reason}
}

func (s Service) reach(ctx context.Context, point, jobID, identity string) error {
	if s.Barrier == nil {
		return nil
	}
	return s.Barrier.ReachWorkflow(ctx, point, jobID, identity)
}

func (s Service) requireClaim(ctx context.Context) error {
	if s.claimCheck == nil {
		return fmt.Errorf("durable executor claim check is not configured")
	}
	return s.claimCheck(ctx)
}

func (s Service) recordAfterClaim(ctx context.Context, record func() error) error {
	return claimBeforeRecord(ctx, s.requireClaim, record)
}

func claimBeforeRecord(ctx context.Context, claimCheck func(context.Context) error, record func() error) error {
	if err := claimCheck(ctx); err != nil {
		return err
	}
	return record()
}

func validatePullIdentity(job coding.Job, pull githubapi.PullRequest, stored *coding.Proposal) error {
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

func validatePull(job coding.Job, pull githubapi.PullRequest, stored *coding.Proposal, title string) error {
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

func Body(job coding.Job, readiness coding.ReadinessAssessment, evidence []core.Evidence, plan *coding.ReviewPlanRecord, runs []coding.ReviewRunView) string {
	digests := make(map[string]string, len(evidence))
	for _, item := range evidence {
		digests[item.ID] = item.Digest
	}
	var lines []string
	lines = append(lines, "## Goal", "", projectGoal(job.Goal), "", "## Exact proposal", "", "- Base: `"+job.BaseBranch+"`", "- Head: `"+job.Branch+"`", "- Revision: `"+job.Revision+"`", "", "## Selected review", "")
	runsByRole := make(map[string]coding.ReviewRunView, len(runs))
	for _, run := range runs {
		if run.JobID == job.ID && run.InputRevision == job.Revision {
			runsByRole[run.Role] = run
		}
	}
	var selectedRoles []policy.Role
	if plan != nil && plan.JobID == job.ID && plan.Revision == job.Revision {
		selectedRoles = plan.Plan.Roles
	}
	for _, role := range selectedRoles {
		run, ok := runsByRole[string(role)]
		if !ok {
			continue
		}
		observedEvidenceID := core.EvidenceID(run.ID, "review-observation")
		feedbackMessageID := core.MessageID(job.ID, core.MessageFromAgent, run.ID)
		lines = append(lines, fmt.Sprintf("- %s: AgentRun `%s` completed with observed Evidence `%s`, sha256 `%s`; feedback Message `%s` handled by an implementation AgentRun", run.Role, run.ID, observedEvidenceID, digests[observedEvidenceID], feedbackMessageID))
	}
	if len(selectedRoles) == 0 {
		lines = append(lines, "- ReviewPolicy selected no agent review.")
	}
	attention := "none"
	if job.WorkflowAttention != "" {
		attention = job.WorkflowAttention
	}
	status := "not ready"
	if readiness.Ready {
		status = "ready"
	}
	lines = append(lines, "", "## Readiness", "", "- Status: "+status, "- Reason: "+readiness.Reason, "- Remaining attention: "+attention, "- Inspect: `dorf job inspect "+job.ID+"`")
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
	Sandbox   provider.Sandbox
	Workspace string
	Ownership func(context.Context, string) (provider.Ownership, error)
	Run       func(context.Context, []string, []string) ([]byte, []byte, error)
}

const gitCredentialHelper = `!f() { case "$1" in get) printf 'username=%s\npassword=%s\n\n' x-access-token "$DORF_EPHEMERAL_GITHUB_TOKEN";; esac; }; f`

func (r GitRepository) Relation(ctx context.Context, job coding.Job, remote string) (string, error) {
	if !postgres.ValidRevision(remote) {
		return "", &AttentionError{Reason: "remote head is not an exact commit OID"}
	}
	owner, err := r.owner(ctx, job.ID)
	if err != nil {
		return "", err
	}
	present, err := r.Sandbox.Exec(ctx, owner, nil, "git", "-C", r.Workspace, "cat-file", "-e", remote+"^{commit}")
	if err != nil || present.ExitCode != 0 {
		return "", &AttentionError{Reason: fmt.Sprintf("remote head %s is not present in the admitted repository; cannot prove safe ancestry", remote)}
	}
	behind, err := r.Sandbox.Exec(ctx, owner, nil, "git", "-C", r.Workspace, "merge-base", "--is-ancestor", remote, job.Revision)
	if err != nil {
		return "", err
	}
	if behind.ExitCode == 0 {
		return "behind", nil
	}
	if behind.ExitCode != 1 {
		return "", fmt.Errorf("classify remote head ancestry: git merge-base exited %d: %s", behind.ExitCode, boundedStderr(behind.Stderr))
	}
	ahead, err := r.Sandbox.Exec(ctx, owner, nil, "git", "-C", r.Workspace, "merge-base", "--is-ancestor", job.Revision, remote)
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

func (r GitRepository) Push(ctx context.Context, job coding.Job, token string) error {
	if token == "" {
		return fmt.Errorf("repository push requires one ephemeral GitHub App token")
	}
	owner, err := r.owner(ctx, job.ID)
	if err != nil {
		return err
	}
	verify, err := r.Sandbox.Exec(ctx, owner, nil, "bash", "-c", `set -eu
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
	run := r.Run
	if run == nil {
		run = runGit
	}
	isolatedGit := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	if _, stderr, err := run(ctx, isolatedGit, []string{"git", "clone", "--bare", bundle, repository}); err != nil {
		return fmt.Errorf("materialize exact publication objects: %s", sanitize(stderr, token))
	}
	remoteURL := "https://github.com/" + job.GitHubRepository + ".git"
	env := append(isolatedGit, "DORF_EPHEMERAL_GITHUB_TOKEN="+token)
	args := []string{"git", "-c", "credential.helper=", "-c", "credential.helper=" + gitCredentialHelper, "-C", repository, "push", "--porcelain", remoteURL, job.Revision + ":refs/heads/" + job.Branch}
	if _, stderr, err := run(ctx, env, args); err != nil {
		return fmt.Errorf("push exact Revision to recorded head without force: %s", sanitize(stderr, token))
	}
	return nil
}

func (r GitRepository) owner(ctx context.Context, jobID string) (provider.Ownership, error) {
	if r.Ownership == nil {
		return provider.Ownership{}, fmt.Errorf("Sandbox ownership resolver is not configured")
	}
	return r.Ownership(ctx, core.MainSandboxName(jobID))
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
