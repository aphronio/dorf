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
}

func newVerificationStore() *verificationStore {
	return &verificationStore{
		profile: core.SandboxProfile{Name: "local", Harness: "codex"},
		verification: core.ProfileVerification{
			ProfileName: "local", ContractVersion: core.BaseProfileContract,
			SandboxID: "dorf-profile-local", OwnershipNonce: "nonce", AttemptedAt: time.Now(),
		},
	}
}

func (s *verificationStore) BeginSandboxProfileVerification(context.Context, string) (core.SandboxProfile, core.ProfileVerification, error) {
	if !s.verification.ProbeCompletedAt.IsZero() && !s.verification.CleanedAt.IsZero() {
		s.verification = core.ProfileVerification{
			ProfileName: s.profile.Name, ContractVersion: core.BaseProfileContract,
			SandboxID: "dorf-profile-local", OwnershipNonce: "fresh-nonce", AttemptedAt: time.Now(),
		}
	}
	s.profile.Verification = &s.verification
	return s.profile, s.verification, nil
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
func (*verificationSandbox) ProviderRouteURL(context.Context, string) (string, error) { return "", nil }

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

func TestVerifyBaseRejectsFailedOrInexactFileReadProbe(t *testing.T) {
	for _, runtime := range []*verificationSandbox{
		{readErr: errors.New("read failed")},
		{readResult: []byte("changed")},
	} {
		store := newVerificationStore()
		store.profile.Artifact = "exact-artifact"
		runtime.execResult = provider.Result{Stdout: "codex 1.2.3\n"}
		if _, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name); err == nil || runtime.deleteCall != 1 || runtime.present {
			t.Fatalf("runtime=%#v err=%v", runtime, err)
		}
	}
}

func TestVerifyBaseFreshlyProbesAnAlreadyVerifiedProfile(t *testing.T) {
	store := newVerificationStore()
	previous := time.Now().Add(-time.Hour)
	store.verification.ProbeCompletedAt = previous
	store.verification.CleanedAt = previous.Add(time.Second)
	store.verification.HarnessVersion = "codex old"
	runtime := &verificationSandbox{execResult: provider.Result{Stdout: "codex fresh\n"}}
	profile, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name)
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

func TestVerifyBaseRejectsFailedAtomicFileProbe(t *testing.T) {
	store := newVerificationStore()
	runtime := &verificationSandbox{putErr: errors.New("upload unavailable")}
	_, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name)
	if err == nil || !strings.Contains(err.Error(), "atomic file probe") || runtime.deleteCall != 1 || store.profile.BaseVerified() {
		t.Fatalf("profile=%#v runtime=%#v error=%v", store.profile, runtime, err)
	}
}

func TestVerifyBaseResumesCleanupWithoutRepeatingProbe(t *testing.T) {
	store := newVerificationStore()
	store.verification.ProbeCompletedAt = time.Now()
	store.verification.HarnessVersion = "codex 1.2.3"
	runtime := &verificationSandbox{present: true, execErr: errors.New("probe must not repeat")}
	profile, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.BaseVerified() || runtime.createCall != 0 || runtime.deleteCall != 1 {
		t.Fatalf("profile=%#v runtime=%#v", profile, runtime)
	}
}

func TestVerifyBaseCleansFailedProbeAndKeepsProfileUnverified(t *testing.T) {
	store := newVerificationStore()
	runtime := &verificationSandbox{execErr: errors.New("transport failed")}
	if _, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name); err == nil {
		t.Fatal("failed probe was accepted")
	}
	if runtime.deleteCall != 1 || runtime.present || store.verification.CleanedAt.IsZero() || store.errorDetail == "" || store.profile.BaseVerified() {
		t.Fatalf("store=%#v runtime=%#v", store, runtime)
	}
}

func TestVerifyBaseReportsFailedPredicateDetail(t *testing.T) {
	store := newVerificationStore()
	runtime := &verificationSandbox{execResult: provider.Result{ExitCode: 1, Stderr: "required command is missing: rg\n"}}
	_, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name)
	if err == nil || !strings.Contains(err.Error(), "required command is missing: rg") {
		t.Fatalf("error=%v", err)
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
			_, err := VerifyBase(context.Background(), store, func(core.SandboxProfile) (provider.Sandbox, error) { return runtime, nil }, store.profile.Name)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("error=%v, want %q", err, test.wantDetail)
			}
			if store.profile.BaseVerified() {
				t.Fatalf("failed cleanup certified profile: %#v", store.profile)
			}
		})
	}
}
