package profile

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type verificationStore struct {
	profile      core.SandboxProfile
	verification core.ProfileVerification
	errorDetail  string
	cleanupErr   error
	lockErr      error
	lockCalls    int
	beginCalls   int
}

func newVerificationStore() *verificationStore {
	profile := core.SandboxProfile{Name: "local", Harness: "codex", Artifact: "exact-artifact"}
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	return &verificationStore{
		profile: profile,
		verification: core.ProfileVerification{
			ProfileName: "local", ContractVersion: core.BaseProfileContract, DefinitionHash: profile.DefinitionHash,
			SandboxID: "dorf-profile-local", OwnershipNonce: "nonce", AttemptedAt: time.Now(),
		},
	}
}

func (s *verificationStore) WithSandboxProfileVerification(ctx context.Context, _ string, work func(context.Context) error) error {
	s.lockCalls++
	if s.lockErr != nil {
		return s.lockErr
	}
	return work(ctx)
}

func (s *verificationStore) BeginSandboxProfileVerification(context.Context, string) (core.SandboxProfile, core.ProfileVerification, error) {
	s.beginCalls++
	if !s.verification.ProbeCompletedAt.IsZero() && !s.verification.CleanedAt.IsZero() {
		s.verification = core.ProfileVerification{
			ProfileName: s.profile.Name, ContractVersion: core.BaseProfileContract, DefinitionHash: s.profile.DefinitionHash,
			SandboxID: "dorf-profile-local", OwnershipNonce: "fresh-nonce", AttemptedAt: time.Now(),
		}
	}
	s.profile.Verification = &s.verification
	return s.profile, s.verification, nil
}

func TestVerifyBaseRefusesBeforeBeginningWithoutExclusiveOwnership(t *testing.T) {
	store := newVerificationStore()
	store.lockErr = errors.New("Sandbox profile verification is already running")
	runtimeBuilt := false
	_, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) {
		runtimeBuilt = true
		return &verificationSandbox{}, nil
	}, store.profile.Name)
	if err == nil || !strings.Contains(err.Error(), "already running") || store.lockCalls != 1 || store.beginCalls != 0 || runtimeBuilt {
		t.Fatalf("lock calls=%d begin calls=%d runtime built=%v err=%v", store.lockCalls, store.beginCalls, runtimeBuilt, err)
	}
}
func (s *verificationStore) RecordSandboxProfileProbe(_ context.Context, verification core.ProfileVerification, version string) error {
	s.verification = verification
	s.verification.HarnessVersion = version
	s.verification.ProbeCompletedAt = time.Now()
	return nil
}
func (s *verificationStore) RecordSandboxProfileVerificationCleanup(_ context.Context, verification core.ProfileVerification) error {
	if s.cleanupErr != nil {
		return s.cleanupErr
	}
	s.verification.CleanedAt = time.Now()
	s.profile.Verification = &s.verification
	return nil
}
func (s *verificationStore) RecordSandboxProfileVerificationError(_ context.Context, _ core.ProfileVerification, err error) error {
	s.errorDetail = err.Error()
	return nil
}
func (s *verificationStore) SandboxProfile(context.Context, string) (core.SandboxProfile, error) {
	s.profile.Verification = &s.verification
	return s.profile, nil
}

type verificationSandbox struct {
	present             bool
	createCall          int
	deleteCall          int
	deleteErr           error
	deleteLeavesPresent bool
	presentErr          error
	putCall             int
	putErr              error
	readCall            int
	readResult          []byte
	readErr             error
	execResult          provider.Result
	execErr             error
}

func (s *verificationSandbox) Workspace() string { return "/workspace/job" }
func (s *verificationSandbox) ReconcileOwnedCreate(context.Context, provider.Ownership) error {
	s.createCall++
	s.present = true
	return nil
}
func (*verificationSandbox) AttestOwnership(context.Context, provider.Ownership) error { return nil }
func (*verificationSandbox) AttachReviewMetadata(context.Context, provider.Ownership, provider.ReviewMetadata) error {
	return nil
}
func (s *verificationSandbox) OwnedPresent(context.Context, provider.Ownership) (bool, error) {
	return s.present, s.presentErr
}
func (s *verificationSandbox) DeleteOwned(context.Context, provider.Ownership) error {
	s.deleteCall++
	if s.deleteErr == nil && !s.deleteLeavesPresent {
		s.present = false
	}
	return s.deleteErr
}
func (*verificationSandbox) AttestReview(context.Context, provider.Ownership, provider.ReviewMetadata) error {
	return nil
}
func (s *verificationSandbox) PutFile(context.Context, provider.Ownership, string, []byte) error {
	s.putCall++
	return s.putErr
}
func (s *verificationSandbox) ReadFile(ctx context.Context, owner provider.Ownership, relativePath string) ([]byte, error) {
	s.readCall++
	if s.readErr != nil {
		return nil, s.readErr
	}
	if s.readResult != nil {
		return append([]byte(nil), s.readResult...), nil
	}
	return []byte{'d', 0, 'o', 'r', 'f', 0xff, '\n'}, nil
}
func (s *verificationSandbox) Exec(context.Context, provider.Ownership, []byte, ...string) (provider.Result, error) {
	return s.execResult, s.execErr
}
func (*verificationSandbox) Endpoint(context.Context, provider.Ownership, int) (provider.Endpoint, error) {
	return provider.NewEndpoint("", "", http.Header{}), nil
}
func (*verificationSandbox) ProviderRouteURL(context.Context) (string, error) { return "", nil }

func verifyBase(store *verificationStore, runtime *verificationSandbox) (core.SandboxProfile, error) {
	return VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name)
}

func TestVerifyBaseRecordsProbeAndExactCleanup(t *testing.T) {
	store := newVerificationStore()
	store.profile.Artifact = "exact-artifact"
	runtime := &verificationSandbox{execResult: provider.Result{Stdout: "codex 1.2.3\n"}}
	observedArtifact := ""
	profile, err := VerifyBase(context.Background(), store, func(profile core.SandboxProfile) (provider.Sandbox, error) {
		observedArtifact = profile.Artifact
		return runtime, nil
	}, store.profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.BaseVerified() || observedArtifact != store.profile.Artifact || runtime.createCall != 1 || runtime.putCall != 2 || runtime.readCall != 1 || runtime.deleteCall != 1 || runtime.present {
		t.Fatalf("profile=%#v runtime=%#v", profile, runtime)
	}
}

func TestVerifyBaseFreshlyProbesAnAlreadyVerifiedProfile(t *testing.T) {
	store := newVerificationStore()
	previous := time.Now().Add(-time.Hour)
	store.verification.ProbeCompletedAt = previous
	store.verification.CleanedAt = previous.Add(time.Second)
	store.verification.HarnessVersion = "codex old"
	runtime := &verificationSandbox{execResult: provider.Result{Stdout: "codex fresh\n"}}
	profile, err := verifyBase(store, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.BaseVerified() || runtime.createCall != 1 || runtime.putCall != 2 || runtime.readCall != 1 || runtime.deleteCall != 1 {
		t.Fatalf("profile=%#v runtime=%#v", profile, runtime)
	}
	if !store.verification.AttemptedAt.After(previous) || store.verification.HarnessVersion != "codex fresh" || store.verification.OwnershipNonce != "fresh-nonce" {
		t.Fatalf("verification was not refreshed: %#v", store.verification)
	}
}

func TestVerifyBaseResumesCleanupWithoutRepeatingProbe(t *testing.T) {
	store := newVerificationStore()
	store.verification.ProbeCompletedAt = time.Now()
	store.verification.HarnessVersion = "codex 1.2.3"
	runtime := &verificationSandbox{present: true, execErr: errors.New("probe must not repeat")}
	profile, err := verifyBase(store, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.BaseVerified() || runtime.createCall != 0 || runtime.deleteCall != 1 {
		t.Fatalf("profile=%#v runtime=%#v", profile, runtime)
	}
}

func TestVerifyBaseCleansFailedProbesAndKeepsProfileUnverified(t *testing.T) {
	for name, test := range map[string]struct {
		runtime *verificationSandbox
		want    string
	}{
		"read failure":     {&verificationSandbox{readErr: errors.New("read failed")}, "read failed"},
		"inexact read":     {&verificationSandbox{readResult: []byte("changed")}, "exact"},
		"atomic upload":    {&verificationSandbox{putErr: errors.New("upload unavailable")}, "atomic file probe"},
		"transport":        {&verificationSandbox{execErr: errors.New("transport failed")}, "transport failed"},
		"predicate detail": {&verificationSandbox{execResult: provider.Result{ExitCode: 1, Stderr: "required command is missing: rg\n"}}, "required command is missing: rg"},
	} {
		t.Run(name, func(t *testing.T) {
			store := newVerificationStore()
			test.runtime.execResult.Stdout = "codex 1.2.3\n"
			_, err := verifyBase(store, test.runtime)
			if err == nil || !strings.Contains(err.Error(), test.want) || test.runtime.deleteCall != 1 || test.runtime.present || store.verification.CleanedAt.IsZero() || store.errorDetail == "" || store.profile.BaseVerified() {
				t.Fatalf("store=%#v runtime=%#v error=%v, want %q", store, test.runtime, err, test.want)
			}
		})
	}
}

func TestVerifyBaseRequiresConfirmedCleanup(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*verificationStore, *verificationSandbox)
		wantDetail string
	}{
		{name: "delete fails", configure: func(_ *verificationStore, runtime *verificationSandbox) {
			runtime.deleteErr = errors.New("delete unavailable")
		}, wantDetail: "delete verification Sandbox"},
		{name: "absence check fails", configure: func(_ *verificationStore, runtime *verificationSandbox) {
			runtime.presentErr = errors.New("inventory unavailable")
		}, wantDetail: "confirm verification Sandbox absence"},
		{name: "resource remains", configure: func(_ *verificationStore, runtime *verificationSandbox) {
			runtime.deleteLeavesPresent = true
		}, wantDetail: "remains after exact deletion"},
		{name: "receipt write fails", configure: func(store *verificationStore, _ *verificationSandbox) {
			store.cleanupErr = errors.New("receipt unavailable")
		}, wantDetail: "record profile verification cleanup"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newVerificationStore()
			store.verification.ProbeCompletedAt = time.Now()
			store.verification.HarnessVersion = "codex 1.2.3"
			runtime := &verificationSandbox{present: true}
			test.configure(store, runtime)
			_, err := verifyBase(store, runtime)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("error=%v, want %q", err, test.wantDetail)
			}
			if store.profile.BaseVerified() {
				t.Fatalf("failed cleanup certified profile: %#v", store.profile)
			}
		})
	}
}
