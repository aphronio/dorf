// Package profile owns Dorf's functional qualification of one exact Sandbox
// provider, artifact, and Harness combination.
package profile

import (
	"context"
	"fmt"
	"strings"

	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/spine"
)

type Store interface {
	BeginSandboxProfileVerification(context.Context, string) (spine.SandboxProfile, spine.ProfileVerification, error)
	RecordSandboxProfileProbe(context.Context, spine.ProfileVerification, string) error
	RecordSandboxProfileVerificationCleanup(context.Context, spine.ProfileVerification) error
	RecordSandboxProfileVerificationError(context.Context, spine.ProfileVerification, error) error
	SandboxProfile(context.Context, string) (spine.SandboxProfile, error)
}

type RuntimeFactory func(spine.SandboxProfile) (provider.Sandbox, error)

// VerifyBase reconciles one disposable provider resource, executes Dorf's
// base-1 functional probe, and confirms exact deletion before the profile may
// admit Jobs. A retry reuses the durable ownership identity.
func VerifyBase(ctx context.Context, store Store, runtimeForProfile RuntimeFactory, name string) (spine.SandboxProfile, error) {
	profile, verification, err := store.BeginSandboxProfileVerification(ctx, name)
	if err != nil {
		return spine.SandboxProfile{}, err
	}
	if profile.BaseVerified() {
		return profile, nil
	}
	if runtimeForProfile == nil {
		return spine.SandboxProfile{}, fmt.Errorf("Sandbox profile runtime construction is not configured")
	}
	runtime, err := runtimeForProfile(profile)
	if err != nil {
		failure := fmt.Errorf("construct verification runtime for Sandbox profile %q: %w", profile.Name, err)
		if recordErr := store.RecordSandboxProfileVerificationError(ctx, verification, failure); recordErr != nil {
			return spine.SandboxProfile{}, fmt.Errorf("%v; record verification failure: %w", failure, recordErr)
		}
		return spine.SandboxProfile{}, failure
	}
	owner := provider.Ownership{
		JobID: "profile:" + profile.Name, SandboxID: verification.SandboxID,
		OwnershipNonce: verification.OwnershipNonce,
	}
	if verification.ProbeCompletedAt.IsZero() {
		if err := runtime.ReconcileOwnedCreate(ctx, owner); err != nil {
			return spine.SandboxProfile{}, failAndClean(ctx, store, runtime, verification, owner, fmt.Errorf("create verification Sandbox: %w", err))
		}
		version, err := runBaseProbe(ctx, runtime, owner, profile.Harness)
		if err != nil {
			return spine.SandboxProfile{}, failAndClean(ctx, store, runtime, verification, owner, err)
		}
		if err := store.RecordSandboxProfileProbe(ctx, verification, version); err != nil {
			return spine.SandboxProfile{}, failAndClean(ctx, store, runtime, verification, owner, fmt.Errorf("record profile probe: %w", err))
		}
	}
	if err := cleanVerificationSandbox(ctx, runtime, owner); err != nil {
		failure := err
		_ = store.RecordSandboxProfileVerificationError(ctx, verification, failure)
		return spine.SandboxProfile{}, failure
	}
	if err := store.RecordSandboxProfileVerificationCleanup(ctx, verification); err != nil {
		return spine.SandboxProfile{}, fmt.Errorf("record profile verification cleanup: %w", err)
	}
	return store.SandboxProfile(ctx, profile.Name)
}

func runBaseProbe(ctx context.Context, runtime provider.Sandbox, owner provider.Ownership, harness string) (string, error) {
	script := `set -eu
workspace=$1
harness=$2
fail() { printf '%s\n' "$1" >&2; exit 1; }
test -d "$workspace" || fail "workspace does not exist: $workspace"
test -w "$workspace" || fail "workspace is not writable: $workspace"
probe="$workspace/.dorf-profile-probe"
: > "$probe" || fail "workspace write probe failed: $workspace"
rm -f -- "$probe"
command -v bash >/dev/null || fail "required command is missing: bash"
command -v git >/dev/null || fail "required command is missing: git"
command -v rg >/dev/null || fail "required command is missing: rg"
command -v "$harness" >/dev/null || fail "required Harness command is missing: $harness"
"$harness" --version`
	result, err := runtime.Exec(ctx, owner, nil, "bash", "-lc", script, "dorf-profile-base-1", runtime.Workspace(), harness)
	if err != nil {
		return "", fmt.Errorf("run Dorf %s profile probe: %w", spine.BaseProfileContract, err)
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("exit %d", result.ExitCode)
		}
		return "", fmt.Errorf("Dorf %s profile probe failed: %s", spine.BaseProfileContract, detail)
	}
	version := strings.TrimSpace(result.Stdout)
	if version == "" || strings.Contains(version, "\n") {
		return "", fmt.Errorf("Dorf %s profile probe returned an invalid Harness version", spine.BaseProfileContract)
	}
	return version, nil
}

func failAndClean(ctx context.Context, store Store, runtime provider.Sandbox, verification spine.ProfileVerification, owner provider.Ownership, failure error) error {
	cleanupErr := cleanVerificationSandbox(ctx, runtime, owner)
	if cleanupErr == nil {
		cleanupErr = store.RecordSandboxProfileVerificationCleanup(ctx, verification)
	}
	if cleanupErr != nil {
		failure = fmt.Errorf("%v; cleanup verification Sandbox: %w", failure, cleanupErr)
	}
	if recordErr := store.RecordSandboxProfileVerificationError(ctx, verification, failure); recordErr != nil {
		return fmt.Errorf("%v; record verification failure: %w", failure, recordErr)
	}
	return failure
}

func cleanVerificationSandbox(ctx context.Context, runtime provider.Sandbox, owner provider.Ownership) error {
	if err := runtime.DeleteOwned(ctx, owner); err != nil {
		return fmt.Errorf("delete verification Sandbox: %w", err)
	}
	present, err := runtime.OwnedPresent(ctx, owner)
	if err != nil {
		return fmt.Errorf("confirm verification Sandbox absence: %w", err)
	}
	if present {
		return fmt.Errorf("verification Sandbox remains after exact deletion")
	}
	return nil
}
