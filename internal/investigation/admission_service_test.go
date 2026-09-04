package investigation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

type admissionServiceStore struct {
	job          core.Job
	source       Source
	profile      core.SandboxProfile
	profileErr   error
	admitted     Admission
	hideJobOnce  bool
	conflictOnce bool
}

func (s *admissionServiceStore) JobExists(_ context.Context, id string) (bool, error) {
	if s.hideJobOnce {
		s.hideJobOnce = false
		return false, nil
	}
	return s.job.ID == id, nil
}

func (s *admissionServiceStore) Job(_ context.Context, _ string) (core.Job, error) { return s.job, nil }

func (s *admissionServiceStore) CodebaseInvestigationSource(_ context.Context, _ string) (Source, error) {
	return s.source, nil
}

func (s *admissionServiceStore) SandboxProfile(_ context.Context, _ string) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) DefaultSandboxProfile(_ context.Context) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) AdmitInvestigation(_ context.Context, input Admission, _ string) (core.Job, bool, error) {
	s.admitted = input
	if s.conflictOnce {
		s.conflictOnce = false
		return core.Job{}, false, ErrAdmissionConflict
	}
	created := s.job.ID == ""
	if created {
		s.job = core.Job{
			ID: core.JobID(input.AdmissionKey), CurrentTaskID: "task-retained", AdmissionKey: input.AdmissionKey,
			Workflow: input.Workflow, WorkflowRevision: input.WorkflowRevision, Goal: input.Goal,
			SandboxProfile: input.SandboxProfile, ProviderConnection: input.ProviderConnection,
			Model: input.Model, ReasoningEffort: input.ReasoningEffort, AdmissionOpen: true,
		}
		s.source = input.Source
		s.source.JobID = s.job.ID
	} else {
		expected := Admission{
			JobAdmission: core.JobAdmission{
				AdmissionKey: s.job.AdmissionKey, Workflow: s.job.Workflow, WorkflowRevision: s.job.WorkflowRevision,
				Goal: s.job.Goal, SandboxProfile: s.job.SandboxProfile, ProviderConnection: s.job.ProviderConnection,
				Model: s.job.Model, ReasoningEffort: s.job.ReasoningEffort,
			},
			Source: s.source,
		}
		expected.Source.JobID = ""
		if expected != input {
			return core.Job{}, false, ErrAdmissionConflict
		}
	}
	return s.job, created, nil
}

type admissionServiceProvider struct {
	err               error
	defaultModelCalls int
}

func (p *admissionServiceProvider) DefaultConnection() (string, error) {
	return "primary", p.err
}

func (p *admissionServiceProvider) DefaultModel(string) (string, error) {
	p.defaultModelCalls++
	return "gpt-5.6-sol", p.err
}

func (p *admissionServiceProvider) Check(context.Context, string) error { return p.err }

func TestAdmissionServiceReconcilesFirstAdmissionRaceAndExactReplay(t *testing.T) {
	store := &admissionServiceStore{profile: verifiedAdmissionProfile("cloud", true)}
	provider := &admissionServiceProvider{}
	service := NewAdmissionService(store, "test-queue", provider)
	request := AdmissionRequest{
		AdmissionKey: "investigation-request", Brief: "preserve exact brief",
		Source: Source{Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40)},
	}

	createdJob, created, err := service.Admit(context.Background(), request)
	if err != nil || !created || createdJob.CurrentTaskID == "" {
		t.Fatalf("new admission Job=%#v created=%t err=%v", createdJob, created, err)
	}
	if store.admitted.SandboxProfile != "cloud" || store.admitted.ProviderConnection != "primary" ||
		store.admitted.Model != "gpt-5.6-sol" || provider.defaultModelCalls != 1 || store.admitted.Source != request.Source {
		t.Fatalf("resolved admission=%#v", store.admitted)
	}
	store.hideJobOnce, store.conflictOnce = true, true
	store.profile = verifiedAdmissionProfile("changed-default", true)
	if raced, created, err := service.Admit(context.Background(), request); err != nil || created || raced.ID != createdJob.ID {
		t.Fatalf("first-admission race Job=%#v created=%t err=%v", raced, created, err)
	}

	store.profileErr = errors.New("profile lookup must be skipped")
	provider.err = errors.New("provider lookup must be skipped")
	replayed, created, err := service.Admit(context.Background(), request)
	if err != nil || created || replayed.ID != createdJob.ID || replayed.CurrentTaskID == "" {
		t.Fatalf("exact replay Job=%#v created=%t err=%v", replayed, created, err)
	}
	changed := request
	changed.Source.Revision = strings.Repeat("b", 40)
	if _, _, err := service.Admit(context.Background(), changed); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed replay error=%v", err)
	}
	malformed := request
	malformed.Source.Repository = "http://credentials@example.test/repo"
	if _, _, err := service.Admit(context.Background(), malformed); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("malformed replay error=%v", err)
	}
	store.job.Workflow = "coding-to-proposal"
	if _, _, err := service.Admit(context.Background(), request); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("foreign-kind replay error=%v", err)
	}
}

func TestAdmissionServiceValidatesAndRequiresRemoteGitBeforeExternalAuthority(t *testing.T) {
	valid := AdmissionRequest{
		AdmissionKey: "request", Brief: "brief", Model: "model",
		Source: Source{Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40)},
	}
	tests := map[string]struct {
		mutate     func(*AdmissionRequest)
		profile    core.SandboxProfile
		profileErr error
	}{
		"malformed":          {mutate: func(r *AdmissionRequest) { r.Source.Repository = "not a repository" }},
		"offline remote Git": {profile: verifiedAdmissionProfile("cloud", false)},
		"missing explicit profile": {
			mutate:     func(r *AdmissionRequest) { r.SandboxProfile = "missing" },
			profileErr: errors.New("profile not found"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := valid
			if test.mutate != nil {
				test.mutate(&request)
			}
			store := &admissionServiceStore{profile: test.profile, profileErr: test.profileErr}
			provider := &admissionServiceProvider{err: errors.New("external authority called")}
			_, _, err := NewAdmissionService(store, "test-queue", provider).Admit(context.Background(), request)
			if !errors.Is(err, ErrInvalidAdmission) {
				t.Fatalf("error=%v, want invalid admission", err)
			}
		})
	}
}

func verifiedAdmissionProfile(name string, internet bool) core.SandboxProfile {
	now := time.Unix(1, 0)
	profile := core.SandboxProfile{Name: name, Provider: core.SandboxProviderE2B, E2BAllowInternet: internet}
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	profile.Verification = &core.ProfileVerification{
		ContractVersion: core.BaseProfileContract, DefinitionHash: profile.DefinitionHash, ProbeCompletedAt: now, CleanedAt: now,
	}
	return profile
}
