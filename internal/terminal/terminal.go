package terminal

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/repository"
	policy "github.com/aphronio/dorf/internal/review"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/spine"
)

type Externals struct {
	Sandbox   provider.Sandbox
	Gateway   gateway.Gateway
	Agent     Harness
	Ownership func(context.Context, string) (provider.Ownership, error)
}

func (e Externals) Harness() string { return e.Agent.Name() }

func (e Externals) repository() repository.Manager {
	return repository.Manager{Sandbox: e.Sandbox, Workspace: e.Sandbox.Workspace()}
}

func (e Externals) SandboxCreate(ctx context.Context, job spine.Job, sandbox spine.Sandbox) error {
	if sandbox.JobID != job.ID {
		return fmt.Errorf("Sandbox does not belong to exact Job %s", job.ID)
	}
	return e.Sandbox.ReconcileOwnedCreate(ctx, ownershipMetadata(sandbox))
}

func (e Externals) RepositoryClone(ctx context.Context, job spine.Job, sandbox spine.Sandbox, repository, revision, branch string) error {
	if sandbox.JobID != job.ID || sandbox.ID != spine.MainSandboxName(job.ID) {
		return fmt.Errorf("repository clone requires the exact main Sandbox")
	}
	return e.Sandbox.ReconcileClone(ctx, ownershipMetadata(sandbox), repository, revision, branch)
}

func (e Externals) RepositoryRestore(ctx context.Context, job spine.Job, owned spine.Sandbox, source investigation.Source, contents []byte) error {
	if owned.JobID != job.ID || owned.ID != spine.MainSandboxName(job.ID) || source.JobID != job.ID ||
		source.Kind != investigation.SourceGitBundle || len(contents) == 0 {
		return fmt.Errorf("repository restore requires the exact retained source and main Sandbox")
	}
	owner := ownershipMetadata(owned)
	workspace := e.Sandbox.Workspace()
	bundlePath := "/tmp/dorf/investigation-source.bundle"
	markerPath := "/tmp/dorf/investigation-source"
	marker := fmt.Sprintf("%s\n%s\n%s\n%s\n", job.ID, owned.ID, owned.OwnershipNonce, source.BundleDigest)
	inspectScript := `set -eu
workspace=$1 marker=$2 expected=$3
if test -e "$workspace/.git"; then
  test -f "$marker" && test "$(cat "$marker")" = "$expected"
elif test -e "$marker"; then
  test -f "$marker" && test "$(cat "$marker")" = "$expected"
elif test -e "$workspace"; then
  test -d "$workspace"
  test -z "$(find "$workspace" -mindepth 1 -maxdepth 1 -print -quit)"
fi`
	inspection, err := e.Sandbox.Exec(ctx, owner, nil, "bash", "-c", inspectScript, "dorf-repository-restore-inspect", workspace, markerPath, strings.TrimSuffix(marker, "\n"))
	if err != nil {
		return err
	}
	if inspection.ExitCode != 0 {
		return fmt.Errorf("existing Sandbox workspace is not owned by the admitted retained source")
	}
	if err := e.Sandbox.PutFile(ctx, owner, markerPath, []byte(marker)); err != nil {
		return fmt.Errorf("install retained repository ownership marker: %w", err)
	}
	if err := e.Sandbox.PutFile(ctx, owner, bundlePath, contents); err != nil {
		return fmt.Errorf("transfer retained repository bundle: %w", err)
	}
	script := `set -eu
	workspace=$1 revision=$2 bundle=$3
if test ! -e "$workspace/.git"; then
  mkdir -p "$workspace"
  git -C "$workspace" init --quiet
fi
git -C "$workspace" bundle verify "$bundle" >/dev/null
git -C "$workspace" fetch --quiet --force --no-tags "$bundle" HEAD:refs/dorf/source
	git -C "$workspace" checkout --quiet --detach "$revision"
git -C "$workspace" reset --quiet --hard "$revision"
git -C "$workspace" clean -qffd
git -C "$workspace" remote remove origin 2>/dev/null || true
git -C "$workspace" config --local user.name 'Dorf Agent'
git -C "$workspace" config --local user.email 'dorf-agent@localhost'
test "$(git -C "$workspace" rev-parse HEAD)" = "$revision"
	test -z "$(git -C "$workspace" symbolic-ref -q --short HEAD || true)"
test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
rm -f -- "$bundle"`
	result, err := e.Sandbox.Exec(ctx, owner, nil, "bash", "-c", script, "dorf-repository-restore", workspace, source.Revision, bundlePath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("restore exact retained repository: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (e Externals) RepositorySetup(ctx context.Context, job spine.CodingJob, action spine.Action) (spine.CommandObservation, []spine.DeclaredCheck, error) {
	owner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return spine.CommandObservation{}, nil, err
	}
	manager := e.repository()
	contract, err := manager.LoadContract(ctx, owner)
	if err != nil {
		return spine.CommandObservation{}, nil, err
	}
	observation, err := manager.RunCommand(ctx, owner, action.ID, job.StartingRevision, contract.Prepare)
	checks := make([]spine.DeclaredCheck, 0, len(contract.Checks))
	for _, check := range contract.Checks {
		checks = append(checks, spine.DeclaredCheck{Name: check.Name, Command: check.Command})
	}
	return observation, checks, err
}

func (e Externals) RepositoryRevision(ctx context.Context, job spine.Job, branch, revision string) (spine.RevisionObservation, error) {
	owner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return spine.RevisionObservation{}, err
	}
	return e.repository().ObserveRevision(ctx, owner, branch, revision)
}

func (e Externals) RepositoryCheck(ctx context.Context, job spine.CodingJob, check spine.Check) (spine.CommandObservation, error) {
	owner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return spine.CommandObservation{}, err
	}
	return e.repository().RunCommand(ctx, owner, check.ID, job.Revision, check.Command)
}

func (e Externals) RepositoryChangeFacts(ctx context.Context, job spine.CodingJob) (policy.ChangeFacts, error) {
	owner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return policy.ChangeFacts{}, err
	}
	return e.repository().ChangeFacts(ctx, owner, job.StartingRevision, job.Revision)
}

func (e Externals) PrepareReviewCheckout(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) error {
	if run.SandboxID == "" {
		return fmt.Errorf("preparing a review checkout requires a dedicated reviewer Sandbox")
	}
	if run.InputRevision != job.Revision {
		return fmt.Errorf("review checkout identity conflicts with current Revision")
	}
	workspace := e.Sandbox.Workspace()
	if err := e.Sandbox.AttachReviewMetadata(ctx, ownershipMetadata(run.Sandbox), reviewMetadata(job, run)); err != nil {
		return err
	}
	sourceScript := `set -eu
workspace=$1; revision=$2
	test "$(git -C "$workspace" rev-parse HEAD)" = "$revision"
	test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
	git -C "$workspace" cat-file -e "$revision^{commit}"
	git -C "$workspace" bundle create - HEAD`
	mainOwner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return err
	}
	bundle, err := e.Sandbox.Exec(ctx, mainOwner, nil, "bash", "-c", sourceScript, "dorf-review-source", workspace, run.InputRevision)
	if err != nil {
		return err
	}
	if bundle.ExitCode != 0 || bundle.Stdout == "" {
		return fmt.Errorf("export admitted Git objects for review: %s", strings.TrimSpace(bundle.Stderr))
	}
	bundlePath := "/tmp/dorf-review.bundle"
	if err := e.Sandbox.PutFile(ctx, ownershipMetadata(run.Sandbox), bundlePath, []byte(bundle.Stdout)); err != nil {
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
	result, err := e.Sandbox.Exec(ctx, ownershipMetadata(run.Sandbox), nil, "bash", "-c", targetScript, "dorf-review-checkout", workspace, run.InputRevision, bundlePath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("prepare exact review checkout: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func (e Externals) VerifyReviewCheckout(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) (spine.ReviewCheckoutObservation, error) {
	if run.SandboxID == "" {
		return spine.ReviewCheckoutObservation{}, fmt.Errorf("review AgentRun has no isolated Sandbox")
	}
	if err := e.Sandbox.AttestReview(ctx, ownershipMetadata(run.Sandbox), reviewMetadata(job, run)); err != nil {
		return spine.ReviewCheckoutObservation{}, err
	}
	script := `set -eu
workspace=$1; revision=$2
head=$(git -C "$workspace" rev-parse HEAD)
tree=$(git -C "$workspace" rev-parse 'HEAD^{tree}')
test "$head" = "$revision"
test -z "$(git -C "$workspace" status --porcelain=v1 --untracked-files=all)"
printf '%s %s clean\n' "$head" "$tree"`
	workspace := e.Sandbox.Workspace()
	result, err := e.Sandbox.Exec(ctx, ownershipMetadata(run.Sandbox), nil, "bash", "-c", script, "dorf-review-verify", workspace, run.InputRevision)
	if err != nil || result.ExitCode != 0 {
		return spine.ReviewCheckoutObservation{}, fmt.Errorf("verify exact review checkout after turn: %s", strings.TrimSpace(result.Stderr))
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) != 3 || fields[0] != run.InputRevision || fields[2] != "clean" {
		return spine.ReviewCheckoutObservation{}, fmt.Errorf("review checkout returned malformed verification")
	}
	return spine.ReviewCheckoutObservation{Revision: fields[0], Tree: fields[1]}, nil
}

func (e Externals) ReviewInitialTurn(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) (spine.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	binding, err := e.Agent.StartStrictReviewTurn(ctx, ownershipMetadata(run.Sandbox), e.Sandbox.Workspace(), reviewMetadata(job, run), run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
	binding.ControllerID = reviewControllerID(run)
	return binding, err
}

func (e Externals) ReviewTurns(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) (spine.HarnessHistory, error) {
	binding, err := e.ReviewWait(ctx, job, run, run.TurnID)
	return spine.HarnessHistory{
		Harness: binding.Harness, ThreadID: binding.ThreadID,
		Turns: []spine.HarnessTurn{binding.Turn}, ControllerID: binding.ControllerID,
	}, err
}

func (e Externals) ReviewRecover(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView) (spine.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	binding, err := e.Agent.RecoverStrictReviewTurn(ctx, ownershipMetadata(run.Sandbox), e.Sandbox.Workspace(), reviewMetadata(job, run), run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
	binding.ControllerID = reviewControllerID(run)
	return binding, err
}

func (e Externals) ReviewWait(ctx context.Context, job spine.CodingJob, run spine.ReviewRunView, turnID string) (spine.HarnessBinding, error) {
	input, err := reviewInput(run)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	binding, err := e.Agent.WaitStrictReviewTurn(ctx, ownershipMetadata(run.Sandbox), e.Sandbox.Workspace(), reviewMetadata(job, run), run.ThreadID, turnID, run.SubmissionNonce, input, job.Model, reviewEffort(run.Role, job.ReasoningEffort))
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

func reviewMetadata(job spine.CodingJob, run spine.ReviewRunView) provider.ReviewMetadata {
	return provider.ReviewMetadata{JobID: job.ID, AgentRunID: run.ID, Revision: run.InputRevision, OwnershipNonce: run.Sandbox.OwnershipNonce}
}

func ownershipMetadata(sandbox spine.Sandbox) provider.Ownership {
	return provider.Ownership{JobID: sandbox.JobID, SandboxID: sandbox.ID, OwnershipNonce: sandbox.OwnershipNonce}
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
	baseURL, err = e.Sandbox.ProviderRouteURL(ctx, baseURL)
	if err != nil {
		return err
	}
	route, err := e.Gateway.ReconcileCreate(ctx, job.ProviderConnection, routeConsumer(sandbox), expected.ID)
	if err != nil {
		return err
	}
	if route.ID != expected.ID {
		return fmt.Errorf("provider Gateway returned a foreign Route identity")
	}
	if err := e.Agent.InstallRoute(ctx, ownershipMetadata(sandbox), baseURL, route.APIKey, job.Model); err != nil {
		return err
	}
	return nil
}

func (e Externals) AgentInitialTurn(ctx context.Context, job spine.Job, delivery spine.Delivery, input string) (spine.HarnessBinding, error) {
	owner, err := e.owner(ctx, delivery.AgentRun.SandboxID)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	return e.Agent.StartInitialTurn(ctx, owner, e.Sandbox.Workspace(), delivery.AgentRun.ID, input, job.Model, job.ReasoningEffort)
}

func (e Externals) AgentInitialTurns(ctx context.Context, job spine.Job) (spine.HarnessHistory, error) {
	owner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return spine.HarnessHistory{}, err
	}
	return e.Agent.ReadInitialTurns(ctx, owner, e.Sandbox.Workspace())
}

func (e Externals) AgentTurns(ctx context.Context, job spine.Job, threadID string) (spine.HarnessHistory, error) {
	owner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return spine.HarnessHistory{}, err
	}
	return e.Agent.ReadTurns(ctx, owner, threadID)
}

func (e Externals) AgentSubmit(ctx context.Context, job spine.Job, delivery spine.Delivery, input string) (spine.HarnessBinding, error) {
	owner, err := e.owner(ctx, delivery.AgentRun.SandboxID)
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	return e.Agent.StartTurn(ctx, owner, e.Sandbox.Workspace(), delivery.AgentRun.ThreadID, delivery.AgentRun.ID, input, job.Model, job.ReasoningEffort)
}

func (e Externals) AgentSteer(ctx context.Context, job spine.Job, delivery spine.Delivery) (string, error) {
	owner, err := e.owner(ctx, delivery.AgentRun.SandboxID)
	if err != nil {
		return "", err
	}
	return e.Agent.SteerTurn(ctx, owner, delivery.AgentRun.ThreadID, delivery.Message.TargetTurnID, delivery.AgentRun.ID, delivery.Message.Input)
}

func (e Externals) AgentWait(ctx context.Context, job spine.Job, threadID, turnID string) (spine.HarnessBinding, error) {
	owner, err := e.owner(ctx, spine.MainSandboxName(job.ID))
	if err != nil {
		return spine.HarnessBinding{}, err
	}
	return e.Agent.WaitTurn(ctx, owner, threadID, turnID)
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
		if err := e.Agent.RemoveRoute(ctx, ownershipMetadata(sandbox)); err != nil {
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

func (e Externals) owner(ctx context.Context, sandboxID string) (provider.Ownership, error) {
	if e.Ownership == nil {
		return provider.Ownership{}, fmt.Errorf("Sandbox ownership resolver is not configured")
	}
	return e.Ownership(ctx, sandboxID)
}

var (
	_ spine.Externals                  = Externals{}
	_ spine.RepositoryServiceExternals = Externals{}
	_ spine.CodingServiceExternals     = Externals{}
	_ investigation.Externals          = Externals{}
)
