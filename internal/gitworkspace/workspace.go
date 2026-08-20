package gitworkspace

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	policy "github.com/aphronio/dorf/internal/review"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type Workspace struct {
	Sandbox   provider.Sandbox
	Workspace string
}

type AttentionError struct{ Reason string }

func (e *AttentionError) Error() string         { return e.Reason }
func (e *AttentionError) AttentionNeeded() bool { return true }

// ReconcileClone materializes one exact remote Revision through the neutral
// Sandbox command boundary. Git behavior belongs to this Git workspace module,
// not to Sandbox providers.
func (m Workspace) ReconcileClone(ctx context.Context, owner provider.Ownership, remote, revision, branch string) error {
	if err := m.Sandbox.AttestOwnership(ctx, owner); err != nil {
		return err
	}
	gitDir, err := m.Sandbox.Exec(ctx, owner, nil, "git", "-C", m.Workspace, "rev-parse", "--git-dir")
	if err != nil {
		return err
	}
	if gitDir.ExitCode != 0 {
		clone, err := m.Sandbox.Exec(ctx, owner, nil, "git", "clone", "--no-checkout", remote, m.Workspace)
		if err != nil {
			return err
		}
		if clone.ExitCode != 0 {
			return sandboxCommandFailure("clone repository", clone)
		}
	} else {
		observed, err := m.Sandbox.Exec(ctx, owner, nil, "git", "-C", m.Workspace, "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		if observed.ExitCode != 0 || strings.TrimSpace(observed.Stdout) != remote {
			return fmt.Errorf("existing Sandbox clone origin does not match admitted repository")
		}
		fetched, err := m.Sandbox.Exec(ctx, owner, nil, "git", "-C", m.Workspace, "fetch", "--prune", "origin")
		if err != nil {
			return err
		}
		if fetched.ExitCode != 0 {
			return sandboxCommandFailure("refresh existing Sandbox clone", fetched)
		}
	}
	checkoutArgs := []string{"git", "-C", m.Workspace, "checkout"}
	if branch == "" {
		checkoutArgs = append(checkoutArgs, "--detach", revision)
	} else {
		checkoutArgs = append(checkoutArgs, "-B", branch, revision)
	}
	checkedOut, err := m.Sandbox.Exec(ctx, owner, nil, checkoutArgs...)
	if err != nil {
		return err
	}
	if checkedOut.ExitCode != 0 {
		return sandboxCommandFailure("checkout admitted Revision", checkedOut)
	}
	head, err := m.Sandbox.Exec(ctx, owner, nil, "git", "-C", m.Workspace, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head.ExitCode != 0 || strings.TrimSpace(head.Stdout) != revision {
		return fmt.Errorf("Sandbox HEAD %q does not match admitted Revision %q", strings.TrimSpace(head.Stdout), revision)
	}
	for _, identity := range [][2]string{{"user.name", "Dorf Agent"}, {"user.email", "dorf-agent@localhost"}} {
		configured, err := m.Sandbox.Exec(ctx, owner, nil, "git", "-C", m.Workspace, "config", "--local", identity[0], identity[1])
		if err != nil {
			return err
		}
		if configured.ExitCode != 0 {
			return sandboxCommandFailure("configure repository-local agent commit identity", configured)
		}
	}
	return nil
}

func sandboxCommandFailure(operation string, result provider.Result) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	return fmt.Errorf("%s: %s", operation, detail)
}

func (m Workspace) ChangeFacts(ctx context.Context, owner provider.Ownership, baseRevision, revision string) (policy.ChangeFacts, error) {
	if !fullOID(baseRevision) || !fullOID(revision) {
		return policy.ChangeFacts{}, fmt.Errorf("ChangeFacts require full immutable Git Revisions")
	}
	if err := m.validateGit(ctx, owner, revision); err != nil {
		return policy.ChangeFacts{}, &AttentionError{Reason: err.Error()}
	}
	result, err := m.Sandbox.Exec(ctx, owner, nil, "git", "-C", m.Workspace, "diff", "--name-only", "-z", baseRevision, revision, "--")
	if err != nil {
		return policy.ChangeFacts{}, err
	}
	if result.ExitCode != 0 {
		return policy.ChangeFacts{}, &AttentionError{Reason: "observe exact Git change paths: " + strings.TrimSpace(result.Stderr)}
	}
	parts := strings.Split(result.Stdout, "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return policy.FactsFromPaths(baseRevision, revision, parts)
}

func (m Workspace) ObserveRevision(ctx context.Context, owner provider.Ownership, branch, comparisonBase string) (Observation, error) {
	if !fullOID(comparisonBase) {
		return Observation{}, fmt.Errorf("comparison base must be a full immutable Git Revision")
	}
	result, err := m.Sandbox.Exec(ctx, owner, nil, "bash", "-c", revisionObservationScript, "dorf-observe-revision", m.Workspace, branch, comparisonBase)
	if err != nil {
		return Observation{}, err
	}
	if result.ExitCode != 0 {
		return Observation{}, &AttentionError{Reason: fmt.Sprintf("observe Git Revision needs attention: %s", strings.TrimSpace(result.Stderr))}
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 6 {
		return Observation{}, &AttentionError{Reason: "Git Revision observation is incomplete"}
	}
	startedNS, startErr := strconv.ParseInt(lines[4], 10, 64)
	finishedNS, finishErr := strconv.ParseInt(lines[5], 10, 64)
	if startErr != nil || finishErr != nil || finishedNS < startedNS {
		return Observation{}, &AttentionError{Reason: "Git Revision observation has invalid bounded timing"}
	}
	observation := Observation{ComparisonBase: lines[0], Revision: lines[1], Tree: lines[2], Branch: lines[3], StartedAt: time.Unix(0, startedNS).UTC(), FinishedAt: time.Unix(0, finishedNS).UTC()}
	if observation.ComparisonBase != comparisonBase || observation.Branch != branch || !fullOID(observation.Revision) || !fullOID(observation.Tree) {
		return Observation{}, &AttentionError{Reason: "Git Revision observation conflicts with admitted branch or comparison base"}
	}
	return observation, nil
}

func (m Workspace) validateGit(ctx context.Context, owner provider.Ownership, revision string) error {
	script := "set -eu; cd \"$2\"; head=$(git rev-parse HEAD); test \"$head\" = \"$1\" || { echo \"HEAD=$head expected=$1\" >&2; exit 10; }; branch=$(git symbolic-ref --short HEAD); test -n \"$branch\" || { echo 'detached or unborn branch' >&2; exit 11; }; status=$(git status --porcelain=v1 --untracked-files=all); test -z \"$status\" || { echo \"dirty checkout: $status\" >&2; exit 12; }"
	result, err := m.Sandbox.Exec(ctx, owner, nil, "bash", "-c", script, "dorf-git", revision, m.Workspace)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("Git checkout is not clean at exact Revision %s: %s", revision, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func fullOID(value string) bool {
	return regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`).MatchString(value)
}

const revisionObservationScript = `set -eu
workspace=$1; branch=$2; comparison_base=$3
export GIT_OPTIONAL_LOCKS=0
cd "$workspace"
start=$(date +%s%N)
current_ref=$(git symbolic-ref -q HEAD || true)
if test -n "$branch"; then
  test "$current_ref" = "refs/heads/$branch" || { echo "branch is $current_ref, expected refs/heads/$branch" >&2; exit 21; }
else
  test -z "$current_ref" || { echo "checkout is attached to $current_ref, expected detached HEAD" >&2; exit 20; }
fi
test "$(git cat-file -t "$comparison_base" 2>/dev/null)" = commit || { echo 'comparison base is not an existing commit' >&2; exit 22; }
head=$(git rev-parse --verify HEAD) || { echo 'unborn branch' >&2; exit 23; }
test "$(git cat-file -t "$head" 2>/dev/null)" = commit || { echo 'HEAD is not a commit' >&2; exit 24; }
tree=$(git show -s --format=%T "$head") || { echo 'cannot observe HEAD tree' >&2; exit 25; }
test "$(git cat-file -t "$tree" 2>/dev/null)" = tree || { echo 'HEAD tree is not a tree object' >&2; exit 26; }
test -z "$(git status --porcelain=v1 --untracked-files=all)" || { echo 'checkout is dirty' >&2; exit 27; }
if test "$head" != "$comparison_base"; then
  git merge-base --is-ancestor "$comparison_base" "$head" || { echo 'HEAD does not descend from comparison base' >&2; exit 28; }
fi
test "$(git symbolic-ref -q HEAD || true)" = "$current_ref" && test "$(git rev-parse --verify HEAD)" = "$head" || { echo 'checkout identity changed during observation' >&2; exit 29; }
test -z "$(git status --porcelain=v1 --untracked-files=all)" || { echo 'checkout changed during observation' >&2; exit 30; }
finish=$(date +%s%N)
printf '%s\n%s\n%s\n%s\n%s\n%s\n' "$comparison_base" "$head" "$tree" "$branch" "$start" "$finish"`
