package terminal

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/repository"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

type Externals struct {
	Sandbox incus.Sandbox
	Gateway gateway.Gateway
	Agent   codex.Agent
}

func (Externals) Harness() string { return codex.Harness }

func (e Externals) repository() repository.Manager {
	return repository.Manager{Sandbox: e.Sandbox, Workspace: e.Sandbox.Config.Workspace}
}

func (e Externals) SandboxCreate(ctx context.Context, job spine.Job, sandbox spine.Sandbox) error {
	if sandbox.JobID != job.ID {
		return fmt.Errorf("Sandbox does not belong to exact Job %s", job.ID)
	}
	return e.Sandbox.ReconcileOwnedCreate(ctx, ownershipMetadata(sandbox))
}

func (e Externals) RepositoryClone(ctx context.Context, job spine.Job, sandbox spine.Sandbox) error {
	if sandbox.JobID != job.ID || sandbox.ID != e.Sandbox.Name(job.ID) {
		return fmt.Errorf("repository clone requires the exact main Sandbox")
	}
	return e.Sandbox.ReconcileClone(ctx, sandbox.ID, job.Repository, job.Revision, job.Branch)
}

func (e Externals) RepositorySetup(ctx context.Context, job spine.Job, action spine.Action) (spine.CommandObservation, []spine.DeclaredCheck, error) {
	manager := e.repository()
	contract, err := manager.LoadContract(ctx, e.Sandbox.Name(job.ID))
	if err != nil {
		return spine.CommandObservation{}, nil, err
	}
	observation, err := manager.RunCommand(ctx, e.Sandbox.Name(job.ID), action.ID, job.StartingRevision, contract.Prepare)
	checks := make([]spine.DeclaredCheck, 0, len(contract.Checks))
	for _, check := range contract.Checks {
		checks = append(checks, spine.DeclaredCheck{Name: check.Name, Command: check.Command})
	}
	return observation, checks, err
}

func (e Externals) RepositoryRevision(ctx context.Context, job spine.Job) (spine.RevisionObservation, error) {
	return e.repository().ObserveRevision(ctx, e.Sandbox.Name(job.ID), job.Branch, job.Revision)
}

func (e Externals) RepositoryCheck(ctx context.Context, job spine.Job, check spine.Check) (spine.CommandObservation, error) {
	return e.repository().RunCommand(ctx, e.Sandbox.Name(job.ID), check.ID, job.Revision, check.Command)
}

func (e Externals) RepositoryChangeFacts(ctx context.Context, job spine.Job) (policy.ChangeFacts, error) {
	return e.repository().ChangeFacts(ctx, e.Sandbox.Name(job.ID), job.StartingRevision, job.Revision)
}

func (e Externals) PrepareReviewCheckout(ctx context.Context, job spine.Job, run spine.ReviewRunView) error {
	if run.SandboxID == "" {
		return fmt.Errorf("preparing a review checkout requires a dedicated reviewer Sandbox")
	}
	if run.InputRevision != job.Revision {
		return fmt.Errorf("review checkout identity conflicts with current Revision")
	}
	workspace := e.Sandbox.Config.Workspace
	if err := e.Sandbox.AttachReviewMetadata(ctx, ownershipMetadata(run.Sandbox), reviewMetadata(job, run)); err != nil {
		return err
	}
	sourceScript := `set -eu
workspace=$1; revision=$2
	test "$(git -C "$workspace" rev-parse HEAD)" = "$revision"
	test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
	git -C "$workspace" cat-file -e "$revision^{commit}"
	git -C "$workspace" bundle create - HEAD`
	bundle, err := e.Sandbox.Exec(ctx, e.Sandbox.Name(job.ID), nil, "bash", "-c", sourceScript, "dorf-review-source", workspace, run.InputRevision)
	if err != nil {
		return err
	}
	if bundle.ExitCode != 0 || bundle.Stdout == "" {
		return fmt.Errorf("export admitted Git objects for review: %s", strings.TrimSpace(bundle.Stderr))
	}
	targetScript := `set -eu
workspace=$1; revision=$2; bundle=/tmp/dorf-review.bundle
umask 077
cat > "$bundle"
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
	result, err := e.Sandbox.Exec(ctx, run.SandboxID, []byte(bundle.Stdout), "bash", "-c", targetScript, "dorf-review-checkout", workspace, run.InputRevision)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("prepare exact review checkout: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (e Externals) VerifyReviewCheckout(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.ReviewCheckoutObservation, error) {
	if run.SandboxID == "" {
		return spine.ReviewCheckoutObservation{}, fmt.Errorf("review AgentRun has no isolated Sandbox")
	}
	if err := e.Sandbox.AttestReview(ctx, run.SandboxID, reviewMetadata(job, run)); err != nil {
		return spine.ReviewCheckoutObservation{}, err
	}
	script := `set -eu
workspace=$1; revision=$2
head=$(git -C "$workspace" rev-parse HEAD)
tree=$(git -C "$workspace" rev-parse 'HEAD^{tree}')
test "$head" = "$revision"
test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
printf '%s %s clean\n' "$head" "$tree"`
	workspace := e.Sandbox.Config.Workspace
	result, err := e.Sandbox.Exec(ctx, run.SandboxID, nil, "bash", "-c", script, "dorf-review-verify", workspace, run.InputRevision)
	if err != nil || result.ExitCode != 0 {
		return spine.ReviewCheckoutObservation{}, fmt.Errorf("verify exact review checkout after turn: %s", strings.TrimSpace(result.Stderr))
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) != 3 || fields[0] != run.InputRevision || fields[2] != "clean" {
		return spine.ReviewCheckoutObservation{}, fmt.Errorf("review checkout returned malformed verification")
	}
	return spine.ReviewCheckoutObservation{Revision: fields[0], Tree: fields[1]}, nil
}

func (e Externals) ReviewInitialTurn(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	binding, err := e.Agent.StartStrictReviewTurn(ctx, run.SandboxID, e.Sandbox.Config.Workspace, reviewMetadata(job, run), run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
	binding.ControllerID = reviewControllerID(run)
	return binding, err
}

func (e Externals) ReviewTurns(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.HarnessHistory, error) {
	binding, err := e.ReviewWait(ctx, job, run, run.TurnID)
	return spine.HarnessHistory{
		Harness: binding.Harness, ThreadID: binding.ThreadID,
		Turns: []spine.HarnessTurn{binding.Turn}, ControllerID: binding.ControllerID,
	}, err
}

func (e Externals) ReviewRecover(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	binding, err := e.Agent.RecoverStrictReviewTurn(ctx, run.SandboxID, e.Sandbox.Config.Workspace, reviewMetadata(job, run), run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
	binding.ControllerID = reviewControllerID(run)
	return binding, err
}

func (e Externals) ReviewWait(ctx context.Context, job spine.Job, run spine.ReviewRunView, turnID string) (spine.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	binding, err := e.Agent.WaitStrictReviewTurn(ctx, run.SandboxID, e.Sandbox.Config.Workspace, reviewMetadata(job, run), run.ThreadID, turnID, run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
	binding.ControllerID = reviewControllerID(run)
	return binding, err
}

func reviewControllerID(run spine.ReviewRunView) string {
	return spine.ReviewControllerID(run.ID, run.SandboxID, run.Sandbox.OwnershipNonce)
}

type reviewInputError string

func (e reviewInputError) Error() string         { return string(e) }
func (e reviewInputError) AttentionNeeded() bool { return true }

func reviewInput(run spine.ReviewRunView) (string, error) {
	if run.MessageID == "" || run.Request.ID != run.MessageID || run.Request.JobID != run.JobID || run.Request.FromKind != spine.MessageFromWorkflow || strings.TrimSpace(run.Request.Input) == "" {
		return "", reviewInputError("review AgentRun request Message is missing or conflicts with its workflow input")
	}
	return run.Request.Input, nil
}

func reviewMetadata(job spine.Job, run spine.ReviewRunView) incus.ReviewMetadata {
	return incus.ReviewMetadata{JobID: job.ID, AgentRunID: run.ID, Revision: run.InputRevision, OwnershipNonce: run.Sandbox.OwnershipNonce}
}

func ownershipMetadata(sandbox spine.Sandbox) incus.OwnershipMetadata {
	return incus.OwnershipMetadata{JobID: sandbox.JobID, SandboxID: sandbox.ID, OwnershipNonce: sandbox.OwnershipNonce}
}

func reviewEffort(role, implementationEffort string) string {
	if role == "auth-authority" || role == "critical-boundary" {
		return implementationEffort
	}
	return "medium"
}

func (e Externals) RouteCreate(ctx context.Context, job spine.Job, sandbox spine.Sandbox, expected spine.Route) error {
	if sandbox.JobID != job.ID || expected.SandboxID != sandbox.ID || expected.ID == "" {
		return fmt.Errorf("provider Route does not belong to exact Job Sandbox")
	}
	if err := e.Sandbox.AttestOwnership(ctx, ownershipMetadata(sandbox)); err != nil {
		return err
	}
	baseURL, err := e.Gateway.BaseURL()
	if err != nil {
		return err
	}
	bridgeIPv4, err := e.Sandbox.BridgeIPv4(ctx)
	if err != nil {
		return err
	}
	if err := requireBridgeRoute(baseURL, bridgeIPv4); err != nil {
		return err
	}
	route, err := e.Gateway.ReconcileCreate(ctx, job.ProviderConnection, routeConsumer(sandbox), expected.ID)
	if err != nil {
		return err
	}
	if route.ID != expected.ID {
		return fmt.Errorf("provider Gateway returned a foreign Route identity")
	}
	if err := e.Sandbox.InstallRoute(ctx, sandbox.ID, route.BaseURL, route.APIKey); err != nil {
		return err
	}
	return nil
}

func (e Externals) AgentInitialTurn(ctx context.Context, job spine.Job, delivery spine.Delivery) (spine.HarnessBinding, error) {
	return e.Agent.StartInitialTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, delivery.AgentRun.ID, codingTurnInput(job, delivery), job.Model, job.ReasoningEffort)
}

func (e Externals) AgentInitialTurns(ctx context.Context, job spine.Job) (spine.HarnessHistory, error) {
	return e.Agent.ReadInitialTurns(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace)
}

func (e Externals) AgentTurns(ctx context.Context, job spine.Job, threadID string) (spine.HarnessHistory, error) {
	return e.Agent.ReadTurns(ctx, e.Sandbox.Name(job.ID), threadID)
}

func (e Externals) AgentSubmit(ctx context.Context, job spine.Job, delivery spine.Delivery) (spine.HarnessBinding, error) {
	return e.Agent.StartTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, delivery.AgentRun.ThreadID, delivery.AgentRun.ID, codingTurnInput(job, delivery), job.Model, job.ReasoningEffort)
}

func codingTurnInput(job spine.Job, delivery spine.Delivery) string {
	if delivery.AgentRun.Role != "implement" {
		return delivery.Message.Input
	}
	return fmt.Sprintf("%s\n\nDorf coding workflow contract: work on branch %q from accepted Revision %s. Before returning control, commit every intended workspace change. You may create one commit or several. Leave the checkout clean, with final HEAD on that branch and descending from the accepted Revision. If this input explicitly concludes that no code change is warranted, leave HEAD unchanged and the checkout clean.", delivery.Message.Input, job.Branch, job.Revision)
}

func (e Externals) AgentSteer(ctx context.Context, job spine.Job, delivery spine.Delivery) (string, error) {
	return e.Agent.SteerTurn(ctx, e.Sandbox.Name(job.ID), delivery.AgentRun.ThreadID, delivery.Message.TargetTurnID, delivery.AgentRun.ID, delivery.Message.Input)
}

func (e Externals) AgentWait(ctx context.Context, job spine.Job, threadID, turnID string) (spine.HarnessBinding, error) {
	return e.Agent.WaitTurn(ctx, e.Sandbox.Name(job.ID), threadID, turnID)
}

func (e Externals) RouteRevoke(ctx context.Context, job spine.Job, sandbox spine.Sandbox, route spine.Route) error {
	if sandbox.JobID != job.ID || route.SandboxID != sandbox.ID || route.ID == "" {
		return fmt.Errorf("Route cleanup has no exact Job-owned identity")
	}
	if err := e.Gateway.RevokeExact(ctx, routeConsumer(sandbox), route.ID); err != nil {
		return err
	}
	present, presentErr := e.Sandbox.OwnedPresent(ctx, ownershipMetadata(sandbox))
	if presentErr != nil {
		return presentErr
	}
	if present {
		if err := e.Sandbox.RemoveRoute(ctx, sandbox.ID); err != nil {
			return err
		}
	}
	return nil
}

func (e Externals) SandboxDelete(ctx context.Context, job spine.Job, sandbox spine.Sandbox) error {
	if sandbox.JobID != job.ID || sandbox.ID == "" {
		return fmt.Errorf("Sandbox cleanup has no exact Job-owned identity")
	}
	return e.Sandbox.DeleteOwned(ctx, ownershipMetadata(sandbox))
}

func routeConsumer(sandbox spine.Sandbox) string { return "sandbox:" + sandbox.ID }

func requireBridgeRoute(baseURL, bridgeIPv4 string) error {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("provider route URL is invalid: %w", err)
	}
	address := net.ParseIP(parsed.Hostname())
	bridge := net.ParseIP(bridgeIPv4)
	if parsed.Scheme != "http" || address == nil || address.To4() == nil || bridge == nil || bridge.To4() == nil || !bridge.IsPrivate() || bridge.IsLoopback() || !address.Equal(bridge) {
		return fmt.Errorf("provider route must use configured Incus bridge IPv4 %s", bridgeIPv4)
	}
	return nil
}

var _ spine.ServiceExternals = Externals{}
