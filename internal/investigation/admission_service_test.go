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
	admitCalls   int
	profileCalls int
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
	s.profileCalls++
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) DefaultSandboxProfile(_ context.Context) (core.SandboxProfile, error) {
	s.profileCalls++
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) AdmitInvestigation(_ context.Context, input Admission) (core.Job, bool, error) {
	s.admitCalls++
	s.admitted = input
	if s.conflictOnce {
		s.conflictOnce = false
		return core.Job{}, false, ErrAdmissionConflict
	}
	created := s.job.ID == ""
	if created {
		s.job = core.Job{
			ID: core.JobID(input.AdmissionKey), AdmissionKey: input.AdmissionKey,
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

type admissionServiceScheduler struct{ calls int }

func (s *admissionServiceScheduler) ScheduleJobTask(_ context.Context, job core.Job, taskName, taskKey string) (core.Job, error) {
	s.calls++
	if taskName != TaskName || taskKey != TaskKey(job.ID) {
		return core.Job{}, errors.New("wrong investigation task identity")
	}
	job.CurrentTaskID = "task-retained"
	return job, nil
}

type admissionServiceProvider struct {
	defaultCalls int
	checkCalls   int
	err          error
}

func (p *admissionServiceProvider) DefaultConnection() (string, error) {
	p.defaultCalls++
	return "primary", p.err
}

func (p *admissionServiceProvider) Check(context.Context, string) error {
	p.checkCalls++
	return p.err
}

func TestAdmissionServiceSelectsAuthorityOnceAndReconcilesExactReplay(t *testing.T) {
	store := &admissionServiceStore{profile: verifiedAdmissionProfile("cloud", true)}
	scheduler := &admissionServiceScheduler{}
	provider := &admissionServiceProvider{}
	service := NewAdmissionService(store, scheduler, provider)
	request := AdmissionRequest{
		AdmissionKey: "investigation-request", Brief: "preserve exact brief", Model: "gpt-5.6-sol",
		Source: Source{Kind: SourceRemote, Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40)},
	}

	createdJob, created, err := service.Admit(context.Background(), request)
	if err != nil || !created || createdJob.CurrentTaskID == "" {
		t.Fatalf("new admission Job=%#v created=%t err=%v", createdJob, created, err)
	}
	if store.admitted.SandboxProfile != "cloud" || store.admitted.ProviderConnection != "primary" || store.admitted.Source != request.Source {
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
	if store.profileCalls != 2 || provider.defaultCalls != 2 || provider.checkCalls != 2 || scheduler.calls != 3 {
		t.Fatalf("replay consulted volatile authority: profile=%d default=%d check=%d schedule=%d",
			store.profileCalls, provider.defaultCalls, provider.checkCalls, scheduler.calls)
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
		Source: Source{Kind: SourceRemote, Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40)},
	}
	tests := map[string]struct {
		request    AdmissionRequest
		profile    core.SandboxProfile
		profileErr error
	}{
		"malformed": {
			request: func() AdmissionRequest {
				request := valid
				request.Source.Repository = "not a repository"
				return request
			}(),
			profile: verifiedAdmissionProfile("cloud", true),
		},
		"offline remote Git": {request: valid, profile: verifiedAdmissionProfile("cloud", false)},
		"missing explicit profile": {
			request: func() AdmissionRequest {
				request := valid
				request.SandboxProfile = "missing"
				return request
			}(),
			profileErr: errors.New("profile not found"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store := &admissionServiceStore{profile: test.profile, profileErr: test.profileErr}
			provider := &admissionServiceProvider{}
			_, _, err := NewAdmissionService(store, &admissionServiceScheduler{}, provider).Admit(context.Background(), test.request)
			if !errors.Is(err, ErrInvalidAdmission) {
				t.Fatalf("error=%v, want invalid admission", err)
			}
			if provider.defaultCalls != 0 || provider.checkCalls != 0 || store.admitCalls != 0 {
				t.Fatalf("invalid admission crossed external authority: default=%d check=%d store=%d",
					provider.defaultCalls, provider.checkCalls, store.admitCalls)
			}
		})
	}
}

func TestAdmissionServiceAllowsRetainedBundleWithoutRemoteGit(t *testing.T) {
	store := &admissionServiceStore{profile: verifiedAdmissionProfile("offline", false)}
	request := AdmissionRequest{
		AdmissionKey: "retained", Brief: "brief", Model: "model",
		Source: Source{Kind: SourceGitBundle, Revision: strings.Repeat("a", 40), BundleDigest: strings.Repeat("b", 64), BundleByteSize: 10},
	}
	if _, created, err := NewAdmissionService(store, &admissionServiceScheduler{}, &admissionServiceProvider{}).Admit(context.Background(), request); err != nil || !created {
		t.Fatalf("retained bundle created=%t err=%v", created, err)
	}
}

func verifiedAdmissionProfile(name string, internet bool) core.SandboxProfile {
	now := time.Unix(1, 0)
	return core.SandboxProfile{
		Name: name, Provider: core.SandboxProviderE2B, E2BAllowInternet: internet,
		Verification: &core.ProfileVerification{
			ContractVersion: core.BaseProfileContract, ProbeCompletedAt: now, CleanedAt: now,
		},
	}
}
