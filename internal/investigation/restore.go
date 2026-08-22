package investigation

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type RestoreTransport interface {
	PutFile(context.Context, provider.Ownership, string, []byte) error
	Exec(context.Context, provider.Ownership, []byte, ...string) (provider.Result, error)
}

// RetainedRestore materializes the exact retained bundle owned by one
// investigation. Its marker makes reconciliation refuse foreign workspace
// contents rather than treating an available Sandbox as disposable.
type RetainedRestore struct {
	Transport RestoreTransport
	Workspace string
}

func (r RetainedRestore) Reconcile(ctx context.Context, job core.Job, owned core.Sandbox, source Source, contents []byte) error {
	if owned.JobID != job.ID || owned.ID != core.MainSandboxName(job.ID) || source.JobID != job.ID ||
		source.Kind != SourceGitBundle || len(contents) == 0 {
		return fmt.Errorf("repository restore requires the exact retained source and main Sandbox")
	}
	owner := restoreOwnership(owned)
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
	inspection, err := r.Transport.Exec(ctx, owner, nil, "bash", "-c", inspectScript, "dorf-repository-restore-inspect", r.Workspace, markerPath, strings.TrimSuffix(marker, "\n"))
	if err != nil {
		return err
	}
	if inspection.ExitCode != 0 {
		return fmt.Errorf("existing Sandbox workspace is not owned by the admitted retained source")
	}
	if err := r.Transport.PutFile(ctx, owner, markerPath, []byte(marker)); err != nil {
		return fmt.Errorf("install retained repository ownership marker: %w", err)
	}
	if err := r.Transport.PutFile(ctx, owner, bundlePath, contents); err != nil {
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
	result, err := r.Transport.Exec(ctx, owner, nil, "bash", "-c", script, "dorf-repository-restore", r.Workspace, source.Revision, bundlePath)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("restore exact retained repository: %s", strings.TrimSpace(result.Stderr))
	}
	return nil
}

func restoreOwnership(sandbox core.Sandbox) provider.Ownership {
	return provider.Ownership{JobID: sandbox.JobID, SandboxID: sandbox.ID, OwnershipNonce: sandbox.OwnershipNonce}
}
