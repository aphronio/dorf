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

func (e Externals) repository() repository.Manager {
	return repository.Manager{Sandbox: e.Sandbox, Workspace: e.Sandbox.Config.Workspace}
}

func (e Externals) SandboxCreate(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	id, err := e.Sandbox.ReconcileCreate(ctx, job.ID)
	return spine.Receipt{ExternalID: id}, err
}

func (e Externals) RepositoryClone(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	name := e.Sandbox.Name(job.ID)
	err := e.Sandbox.ReconcileClone(ctx, name, job.Repository, job.Revision, job.Branch)
	return spine.Receipt{ExternalID: name + ":" + e.Sandbox.Config.Workspace}, err
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

func (e Externals) RepositoryRevision(ctx context.Context, job spine.Job) (spine.RevisionObservation, []byte, error) {
	return e.repository().ObserveRevision(ctx, e.Sandbox.Name(job.ID), job.Branch, job.Revision)
}

func (e Externals) RepositoryCheck(ctx context.Context, job spine.Job, check spine.Check) (spine.CommandObservation, error) {
	return e.repository().RunCommand(ctx, e.Sandbox.Name(job.ID), check.ID, job.Revision, check.Command)
}

func (e Externals) RepositoryChangeFacts(ctx context.Context, job spine.Job) (policy.ChangeFacts, error) {
	return e.repository().ChangeFacts(ctx, e.Sandbox.Name(job.ID), job.StartingRevision, job.Revision)
}

func (e Externals) ReviewWorkspaceCreate(ctx context.Context, job spine.Job, run spine.ReviewRunView, _ spine.Action) (spine.Receipt, error) {
	if run.ReviewerSandboxID == "" {
		return spine.Receipt{}, fmt.Errorf("review materialization requires a dedicated reviewer Sandbox")
	}
	if run.Revision != job.Revision || run.Workspace != e.Sandbox.Config.Workspace {
		return spine.Receipt{}, fmt.Errorf("review workspace identity conflicts with current Revision or bounded root")
	}
	if err := e.Sandbox.AttestReview(ctx, run.ReviewerSandboxID, reviewMetadata(job, run)); err != nil {
		return spine.Receipt{}, err
	}
	sourceScript := `set -eu
main=$1; workspace=$2; revision=$3
	test "$(git -C "$main" rev-parse HEAD)" = "$revision"
	test -z "$(git -C "$main" status --porcelain=v1 --untracked-files=all)"
	git -C "$main" cat-file -e "$revision^{commit}"
	git -C "$main" bundle create - HEAD`
	bundle, err := e.Sandbox.Exec(ctx, e.Sandbox.Name(job.ID), nil, "bash", "-c", sourceScript, "dorf-review-source", e.Sandbox.Config.Workspace, run.Workspace, run.Revision)
	if err != nil {
		return spine.Receipt{}, err
	}
	if bundle.ExitCode != 0 || bundle.Stdout == "" {
		return spine.Receipt{}, fmt.Errorf("export admitted Git objects for review: %s", strings.TrimSpace(bundle.Stderr))
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
tree=$(git -C "$workspace" rev-parse 'HEAD^{tree}')
test "$head" = "$revision"
test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
rm -f -- "$bundle"
printf '%s %s clean\n' "$head" "$tree"`
	result, err := e.Sandbox.Exec(ctx, run.ReviewerSandboxID, []byte(bundle.Stdout), "bash", "-c", targetScript, "dorf-review-materialize", run.Workspace, run.Revision)
	if err != nil {
		return spine.Receipt{}, err
	}
	if result.ExitCode != 0 {
		return spine.Receipt{}, fmt.Errorf("materialize exact review checkout: %s", strings.TrimSpace(result.Stderr))
	}
	return spine.Receipt{ExternalID: run.Workspace, Outcome: strings.TrimSpace(result.Stdout)}, nil
}

func (e Externals) ReviewSandboxCreate(ctx context.Context, job spine.Job, run spine.ReviewRunView, _ spine.Action) (spine.Receipt, error) {
	if run.ReviewerSandboxID != spine.ReviewSandboxName(run.ID) || run.ReviewerSandboxID == e.Sandbox.Name(job.ID) {
		return spine.Receipt{}, fmt.Errorf("reviewer Sandbox identity is not isolated from the implementation Sandbox")
	}
	id, err := e.Sandbox.ReconcileReviewCreate(ctx, run.ReviewerSandboxID, reviewMetadata(job, run))
	return spine.Receipt{ExternalID: id, Outcome: run.Revision}, err
}

func (e Externals) ReviewRouteCreate(ctx context.Context, job spine.Job, run spine.ReviewRunView, action spine.Action) (spine.Receipt, error) {
	if err := e.Sandbox.AttestReview(ctx, run.ReviewerSandboxID, reviewMetadata(job, run)); err != nil {
		return spine.Receipt{}, err
	}
	baseURL, err := e.Gateway.BaseURL()
	if err != nil {
		return spine.Receipt{}, err
	}
	bridgeIPv4, err := e.Sandbox.BridgeIPv4(ctx)
	if err != nil {
		return spine.Receipt{}, err
	}
	if err := requireBridgeRoute(baseURL, bridgeIPv4); err != nil {
		return spine.Receipt{}, err
	}
	route, err := e.Gateway.ReconcileCreate(ctx, job.ProviderConnection, "review:"+run.ID, action.ID)
	if err != nil {
		return spine.Receipt{}, err
	}
	if err := e.Sandbox.InstallRoute(ctx, run.ReviewerSandboxID, route.BaseURL, route.APIKey); err != nil {
		return spine.Receipt{}, err
	}
	return spine.Receipt{ExternalID: route.ID, Outcome: run.ReviewerSandboxID}, nil
}

func (e Externals) ReviewWorkspaceVerify(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.Receipt, error) {
	if run.ReviewerSandboxID == "" {
		return spine.Receipt{}, fmt.Errorf("legacy implementation-Sandbox review cannot produce isolated post-turn attestation")
	}
	if err := e.Sandbox.AttestReview(ctx, run.ReviewerSandboxID, reviewMetadata(job, run)); err != nil {
		return spine.Receipt{}, err
	}
	script := `set -eu
workspace=$1; revision=$2
head=$(git -C "$workspace" rev-parse HEAD)
tree=$(git -C "$workspace" rev-parse 'HEAD^{tree}')
test "$head" = "$revision"
test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
printf '%s %s clean\n' "$head" "$tree"`
	result, err := e.Sandbox.Exec(ctx, run.ReviewerSandboxID, nil, "bash", "-c", script, "dorf-review-verify", run.Workspace, run.Revision)
	if err != nil || result.ExitCode != 0 {
		return spine.Receipt{}, fmt.Errorf("verify exact reviewer checkout after turn: %s", strings.TrimSpace(result.Stderr))
	}
	return spine.Receipt{ExternalID: run.Workspace, Outcome: strings.TrimSpace(result.Stdout)}, nil
}

func (e Externals) ReviewRouteRevoke(ctx context.Context, job spine.Job, run spine.ReviewRunView, _ spine.Action) (spine.Receipt, error) {
	present, err := e.Sandbox.ReviewPresent(ctx, run.ReviewerSandboxID, reviewMetadata(job, run))
	if err != nil {
		return spine.Receipt{}, err
	}
	expectedRouteID, err := reviewerRouteID(job, run)
	if err != nil {
		return spine.Receipt{}, err
	}
	id, err := e.Gateway.RevokeExact(ctx, "review:"+run.ID, expectedRouteID)
	if err != nil {
		return spine.Receipt{}, err
	}
	if present {
		if err := e.Sandbox.RemoveRoute(ctx, run.ReviewerSandboxID); err != nil {
			return spine.Receipt{}, err
		}
	}
	return spine.Receipt{ExternalID: id, Outcome: "revoked"}, nil
}

func reviewerRouteID(job spine.Job, run spine.ReviewRunView) (string, error) {
	if run.JobID != job.ID || strings.TrimSpace(run.ID) == "" {
		return "", fmt.Errorf("reviewer route cleanup identity does not belong to exact Job %s", job.ID)
	}
	if run.ReviewerRouteID != "" {
		return run.ReviewerRouteID, nil
	}
	createActionID := spine.ScopedActionID(job.ID, spine.ActionRouteCreate, run.ID)
	return gateway.RouteID(createActionID), nil
}

func (e Externals) ReviewSandboxDelete(ctx context.Context, job spine.Job, run spine.ReviewRunView, _ spine.Action) (spine.Receipt, error) {
	err := e.Sandbox.DeleteReview(ctx, run.ReviewerSandboxID, reviewMetadata(job, run))
	return spine.Receipt{ExternalID: run.ReviewerSandboxID, Outcome: "deleted"}, err
}

func (e Externals) ReviewInitialTurn(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.ReviewNativeBinding, error) {
	return e.Agent.StartStrictReviewTurn(ctx, run.ReviewerSandboxID, run.Workspace, reviewMetadata(job, run), run.SubmissionNonce, run.InputContract, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
}

func (e Externals) ReviewTurns(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.ReviewNativeHistory, error) {
	return e.Agent.ReadStrictReviewTurns(ctx, run.ReviewerSandboxID, run.Workspace, reviewMetadata(job, run), run.SessionID, run.SubmissionNonce, run.InputContract, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
}

func (e Externals) ReviewRecover(ctx context.Context, job spine.Job, run spine.ReviewRunView) (spine.ReviewNativeBinding, error) {
	return e.Agent.RecoverStrictReviewTurn(ctx, run.ReviewerSandboxID, run.Workspace, reviewMetadata(job, run), run.SubmissionNonce, run.InputContract, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
}

func (e Externals) ReviewWait(ctx context.Context, job spine.Job, run spine.ReviewRunView, turnID string) (spine.ReviewNativeBinding, error) {
	return e.Agent.WaitStrictReviewTurn(ctx, run.ReviewerSandboxID, run.Workspace, reviewMetadata(job, run), run.SessionID, turnID, run.SubmissionNonce, run.InputContract, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
}

func reviewMetadata(job spine.Job, run spine.ReviewRunView) incus.ReviewMetadata {
	return incus.ReviewMetadata{JobID: job.ID, AgentRunID: run.ID, Revision: run.Revision, OwnershipNonce: run.ReviewerOwnerNonce}
}

func reviewEffort(role, implementationEffort string) string {
	if role == "auth-authority" || role == "critical-boundary" {
		return implementationEffort
	}
	return "medium"
}

func (e Externals) RouteCreate(ctx context.Context, job spine.Job, action spine.Action) (spine.Receipt, error) {
	baseURL, err := e.Gateway.BaseURL()
	if err != nil {
		return spine.Receipt{}, err
	}
	bridgeIPv4, err := e.Sandbox.BridgeIPv4(ctx)
	if err != nil {
		return spine.Receipt{}, err
	}
	if err := requireBridgeRoute(baseURL, bridgeIPv4); err != nil {
		return spine.Receipt{}, err
	}
	route, err := e.Gateway.ReconcileCreate(ctx, job.ProviderConnection, "sandbox:"+job.ID, action.ID)
	if err != nil {
		return spine.Receipt{}, err
	}
	if err := e.Sandbox.InstallRoute(ctx, e.Sandbox.Name(job.ID), route.BaseURL, route.APIKey); err != nil {
		return spine.Receipt{}, err
	}
	return spine.Receipt{ExternalID: route.ID}, nil
}

func (e Externals) AgentInitialTurn(ctx context.Context, job spine.Job, delivery spine.Delivery) (string, spine.NativeTurn, error) {
	return e.Agent.StartInitialTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, delivery.AgentRun.ID, codingTurnInput(job, delivery), job.Model, job.ReasoningEffort)
}

func (e Externals) AgentInitialTurns(ctx context.Context, job spine.Job) (string, []spine.NativeTurn, error) {
	return e.Agent.ReadInitialTurns(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace)
}

func (e Externals) AgentTurns(ctx context.Context, job spine.Job, sessionID string) ([]spine.NativeTurn, error) {
	return e.Agent.ReadTurns(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, sessionID)
}

func (e Externals) AgentSubmit(ctx context.Context, job spine.Job, delivery spine.Delivery) (spine.NativeTurn, error) {
	return e.Agent.StartTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, delivery.AgentRun.SessionID, delivery.AgentRun.ID, codingTurnInput(job, delivery), job.Model, job.ReasoningEffort)
}

func codingTurnInput(job spine.Job, delivery spine.Delivery) string {
	if delivery.AgentRun.Role != "implement" {
		return delivery.Message.Input
	}
	return fmt.Sprintf("%s\n\nDorf coding workflow contract: work on branch %q from accepted Revision %s. Before returning control, commit every intended workspace change. You may create one commit or several. Leave the checkout clean, with final HEAD on that branch and descending from the accepted Revision. If this input explicitly concludes that no code change is warranted, leave HEAD unchanged and the checkout clean.", delivery.Message.Input, job.Branch, job.Revision)
}

func (e Externals) AgentSteer(ctx context.Context, job spine.Job, delivery spine.Delivery) (string, error) {
	return e.Agent.SteerTurn(ctx, e.Sandbox.Name(job.ID), delivery.AgentRun.SessionID, delivery.Message.TargetTurnID, delivery.AgentRun.ID, delivery.Message.Input)
}

func (e Externals) AgentWait(ctx context.Context, job spine.Job, sessionID, turnID string) (spine.NativeTurn, error) {
	return e.Agent.WaitTurn(ctx, e.Sandbox.Name(job.ID), e.Sandbox.Config.Workspace, sessionID, turnID)
}

func (e Externals) RouteRevoke(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	if job.RouteID == "" {
		return spine.Receipt{}, fmt.Errorf("main route cleanup has no recorded exact route ID")
	}
	id, err := e.Gateway.RevokeExact(ctx, "sandbox:"+job.ID, job.RouteID)
	if err != nil {
		return spine.Receipt{}, err
	}
	_ = e.Sandbox.RemoveRoute(ctx, e.Sandbox.Name(job.ID))
	return spine.Receipt{ExternalID: id, Outcome: "revoked"}, nil
}

func (e Externals) SandboxDelete(ctx context.Context, job spine.Job, _ spine.Action) (spine.Receipt, error) {
	name := job.SandboxID
	if name == "" {
		return spine.Receipt{}, fmt.Errorf("main Sandbox cleanup has no recorded exact Incus name")
	}
	err := e.Sandbox.Delete(ctx, name)
	return spine.Receipt{ExternalID: name, Outcome: "deleted"}, err
}

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

var _ spine.ReviewExternals = Externals{}
