package coding

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type ReviewTransport interface {
	Workspace() string
	AttachReviewMetadata(context.Context, provider.Ownership, provider.ReviewMetadata) error
	AttestReview(context.Context, provider.Ownership, provider.ReviewMetadata) error
	PutFile(context.Context, provider.Ownership, string, []byte) error
	Exec(context.Context, provider.Ownership, []byte, ...string) (provider.Result, error)
}

type ReviewHarness interface {
	Name() string
	StartStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string) (core.HarnessBinding, error)
	RecoverStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string) (core.HarnessBinding, error)
	ReadStrictReviewTurn(context.Context, provider.Ownership, string, provider.ReviewMetadata, string, string, string, string, string, string) (core.HarnessBinding, error)
}

// ReviewController owns the isolated checkout and strict Harness boundary for
// coding review. Core continues to own AgentRun custody and recovery.
type ReviewController struct {
	Transport ReviewTransport
	Agent     ReviewHarness
	Ownership func(context.Context, string) (provider.Ownership, error)
}

func (c ReviewController) Harness() string { return c.Agent.Name() }

func (c ReviewController) PrepareReviewCheckout(ctx context.Context, job Job, run ReviewRunView) error {
	if run.SandboxID == "" {
		return fmt.Errorf("preparing a review checkout requires a dedicated reviewer Sandbox")
	}
	if run.InputRevision != job.Revision {
		return fmt.Errorf("review checkout identity conflicts with current Revision")
	}
	workspace := c.Transport.Workspace()
	if err := c.Transport.AttachReviewMetadata(ctx, reviewOwnership(run.Sandbox), reviewMetadata(job, run)); err != nil {
		return err
	}
	sourceScript := `set -eu
workspace=$1; revision=$2
	test "$(git -C "$workspace" rev-parse HEAD)" = "$revision"
	test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
	git -C "$workspace" cat-file -e "$revision^{commit}"
	git -C "$workspace" bundle create - HEAD`
	mainOwner, err := c.owner(ctx, core.MainSandboxName(job.ID))
	if err != nil {
		return err
	}
	bundle, err := c.Transport.Exec(ctx, mainOwner, nil, "bash", "-c", sourceScript, "dorf-review-source", workspace, run.InputRevision)
	if err != nil {
		return err
	}
	if bundle.ExitCode != 0 || bundle.Stdout == "" {
		return fmt.Errorf("export admitted Git objects for review: %s", strings.TrimSpace(bundle.Stderr))
	}
	bundlePath := "/tmp/dorf-review.bundle"
	if err := c.Transport.PutFile(ctx, reviewOwnership(run.Sandbox), bundlePath, []byte(bundle.Stdout)); err != nil {
		return fmt.Errorf("transfer exact review bundle: %w", err)
	}
	targetScript := `set -eu
workspace=$1; revision=$2; bundle=$3
if test ! -e "$workspace/.git"; then
  mkdir -p "$(dirname "$workspace")"
  git clone --no-checkout "$bundle" "$workspace" >/dev/null
  git -C "$workspace" remote remove origin
fi
git -C "$workspace" bundle verify "$bundle" >/dev/null
git -C "$workspace" checkout --detach "$revision" >/dev/null
git -C "$workspace" for-each-ref --format='delete %(refname)' | git -C "$workspace" update-ref --stdin
git -C "$workspace" reflog expire --expire=now --all
git -C "$workspace" gc --prune=now
test -z "$(git -C "$workspace" fsck --strict --unreachable)"
head=$(git -C "$workspace" rev-parse HEAD)
test "$head" = "$revision"
test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
rm -f -- "$bundle"`
	result, err := c.Transport.Exec(ctx, reviewOwnership(run.Sandbox), nil, "bash", "-c", targetScript, "dorf-review-checkout", workspace, run.InputRevision, bundlePath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("prepare exact review checkout: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (c ReviewController) VerifyReviewCheckout(ctx context.Context, job Job, run ReviewRunView) (ReviewCheckoutObservation, error) {
	if run.SandboxID == "" {
		return ReviewCheckoutObservation{}, fmt.Errorf("review AgentRun has no isolated Sandbox")
	}
	if err := c.Transport.AttestReview(ctx, reviewOwnership(run.Sandbox), reviewMetadata(job, run)); err != nil {
		return ReviewCheckoutObservation{}, err
	}
	script := `set -eu
workspace=$1; revision=$2
head=$(git -C "$workspace" rev-parse HEAD)
tree=$(git -C "$workspace" rev-parse 'HEAD^{tree}')
test "$head" = "$revision"
test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
printf '%s %s clean\n' "$head" "$tree"`
	result, err := c.Transport.Exec(ctx, reviewOwnership(run.Sandbox), nil, "bash", "-c", script, "dorf-review-verify", c.Transport.Workspace(), run.InputRevision)
	if err != nil || result.ExitCode != 0 {
		return ReviewCheckoutObservation{}, fmt.Errorf("verify exact review checkout after turn: %s", strings.TrimSpace(result.Stderr))
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) != 3 || fields[0] != run.InputRevision || fields[2] != "clean" {
		return ReviewCheckoutObservation{}, fmt.Errorf("review checkout returned malformed verification")
	}
	return ReviewCheckoutObservation{Revision: fields[0], Tree: fields[1]}, nil
}

func (c ReviewController) ReviewInitialTurn(ctx context.Context, job Job, run ReviewRunView) (core.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return core.HarnessBinding{}, err
	}
	return c.Agent.StartStrictReviewTurn(ctx, reviewOwnership(run.Sandbox), c.Transport.Workspace(), reviewMetadata(job, run), run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
}

func (c ReviewController) ReviewTurns(ctx context.Context, job Job, run ReviewRunView) (core.HarnessHistory, error) {
	input, err := reviewInput(run)
	if err != nil {
		return core.HarnessHistory{}, err
	}
	binding, err := c.Agent.ReadStrictReviewTurn(ctx, reviewOwnership(run.Sandbox), c.Transport.Workspace(), reviewMetadata(job, run), run.ThreadID, run.TurnID, run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
	return core.HarnessHistory{
		Harness: binding.Harness, ThreadID: binding.ThreadID,
		Turns: []core.HarnessTurn{binding.Turn},
	}, err
}

func (c ReviewController) ReviewRecover(ctx context.Context, job Job, run ReviewRunView) (core.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return core.HarnessBinding{}, err
	}
	return c.Agent.RecoverStrictReviewTurn(ctx, reviewOwnership(run.Sandbox), c.Transport.Workspace(), reviewMetadata(job, run), run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
}

type reviewInputError string

func (e reviewInputError) Error() string         { return string(e) }
func (e reviewInputError) AttentionNeeded() bool { return true }

func reviewInput(run ReviewRunView) (string, error) {
	if run.MessageID == "" || run.Request.ID != run.MessageID || run.Request.JobID != run.JobID || run.Request.FromKind != core.MessageFromWorkflow || strings.TrimSpace(run.Request.Input) == "" {
		return "", reviewInputError("review AgentRun request Message is missing or conflicts with its workflow input")
	}
	return run.Request.Input, nil
}

func reviewMetadata(job Job, run ReviewRunView) provider.ReviewMetadata {
	return provider.ReviewMetadata{JobID: job.ID, AgentRunID: run.ID, Revision: run.InputRevision, OwnershipNonce: run.Sandbox.OwnershipNonce}
}

func reviewOwnership(sandbox core.Sandbox) provider.Ownership {
	return provider.Ownership{JobID: sandbox.JobID, SandboxID: sandbox.ID, OwnershipNonce: sandbox.OwnershipNonce}
}

func reviewEffort(role, implementationEffort string) string {
	if role == "auth-authority" || role == "critical-boundary" {
		return implementationEffort
	}
	return "medium"
}

func (c ReviewController) owner(ctx context.Context, sandboxID string) (provider.Ownership, error) {
	if c.Ownership == nil {
		return provider.Ownership{}, fmt.Errorf("Sandbox ownership resolver is not configured")
	}
	return c.Ownership(ctx, sandboxID)
}
