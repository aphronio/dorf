package coding

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
	typed        Job
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

func (s *admissionServiceStore) CodingJob(_ context.Context, _ string) (Job, error) {
	return s.typed, nil
}

func (s *admissionServiceStore) SandboxProfile(_ context.Context, _ string) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) DefaultSandboxProfile(_ context.Context) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) AdmitCoding(_ context.Context, input Admission) (core.Job, bool, error) {
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
		s.typed = Job{
			Job: s.job, Repository: input.Repository, StartingRevision: input.Revision, Revision: input.Revision,
			Branch: input.Branch, GitHubRepository: input.GitHubRepository,
			GitHubInstallation: input.GitHubInstallation, BaseBranch: input.BaseBranch,
		}
	} else {
		expected := Admission{
			JobAdmission: core.JobAdmission{
				AdmissionKey: s.job.AdmissionKey, Workflow: s.job.Workflow, WorkflowRevision: s.job.WorkflowRevision,
				Goal: s.job.Goal, SandboxProfile: s.job.SandboxProfile, ProviderConnection: s.job.ProviderConnection,
				Model: s.job.Model, ReasoningEffort: s.job.ReasoningEffort,
			},
			Repository: s.typed.Repository, Revision: s.typed.StartingRevision, Branch: s.typed.Branch,
			GitHubRepository: s.typed.GitHubRepository, GitHubInstallation: s.typed.GitHubInstallation, BaseBranch: s.typed.BaseBranch,
		}
		if expected != input {
			return core.Job{}, false, ErrAdmissionConflict
		}
	}
	return s.job, created, nil
}

type admissionServiceScheduler struct{}

func (s *admissionServiceScheduler) ScheduleJobTask(_ context.Context, job core.Job, taskName, taskKey string) (core.Job, error) {
	if taskName != TaskName || taskKey != TaskKey(job.ID) {
		return core.Job{}, errors.New("wrong coding task identity")
	}
	job.CurrentTaskID = "task-retained"
	return job, nil
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

type admissionServiceInstallations struct{ err error }

func (g *admissionServiceInstallations) DiscoverInstallation(_ context.Context, repository string) (string, error) {
	if repository != "aphronio/dorf" {
		return "", errors.New("wrong GitHub repository")
	}
	if g.err != nil {
		return "", g.err
	}
	return "42", nil
}

func TestAdmissionServiceReconcilesFirstAdmissionRaceAndExactReplay(t *testing.T) {
	store := &admissionServiceStore{profile: verifiedAdmissionProfile("cloud", true)}
	scheduler := &admissionServiceScheduler{}
	provider := &admissionServiceProvider{}
	installations := &admissionServiceInstallations{}
	service := NewAdmissionService(store, scheduler, provider, installations)
	request := AdmissionRequest{
		AdmissionKey: "coding-request", Goal: "preserve exact goal",
		Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40), BaseBranch: "main",
	}

	createdJob, created, err := service.Admit(context.Background(), request)
	if err != nil || !created || createdJob.CurrentTaskID == "" {
		t.Fatalf("new admission Job=%#v created=%t err=%v", createdJob, created, err)
	}
	if store.admitted.SandboxProfile != "cloud" || store.admitted.ProviderConnection != "primary" ||
		store.admitted.Model != "gpt-5.6-sol" || provider.defaultModelCalls != 1 ||
		store.admitted.GitHubRepository != "aphronio/dorf" || store.admitted.GitHubInstallation != "42" ||
		store.admitted.Branch != "dorf/"+core.JobID(request.AdmissionKey) {
		t.Fatalf("resolved admission=%#v", store.admitted)
	}
	store.hideJobOnce, store.conflictOnce = true, true
	store.profile = verifiedAdmissionProfile("changed-default", true)
	if raced, created, err := service.Admit(context.Background(), request); err != nil || created || raced.ID != createdJob.ID {
		t.Fatalf("first-admission race Job=%#v created=%t err=%v", raced, created, err)
	}

	store.profileErr = errors.New("profile lookup must be skipped")
	provider.err = errors.New("provider lookup must be skipped")
	installations.err = errors.New("GitHub lookup must be skipped")
	replayed, created, err := service.Admit(context.Background(), request)
	if err != nil || created || replayed.ID != createdJob.ID || replayed.CurrentTaskID == "" {
		t.Fatalf("exact replay Job=%#v created=%t err=%v", replayed, created, err)
	}
	changed := request
	changed.BaseBranch = "develop"
	if _, _, err := service.Admit(context.Background(), changed); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed replay error=%v", err)
	}
	malformed := request
	malformed.Repository = "not a repository"
	if _, _, err := service.Admit(context.Background(), malformed); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("malformed replay error=%v", err)
	}
	store.job.Workflow = "codebase-investigation"
	if _, _, err := service.Admit(context.Background(), request); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("foreign-kind replay error=%v", err)
	}
}

func TestAdmissionServiceValidatesAndRequiresRemoteGitBeforeExternalAuthority(t *testing.T) {
	valid := AdmissionRequest{
		AdmissionKey: "request", Goal: "goal", Model: "model", Repository: "https://github.com/aphronio/dorf.git",
		Revision: strings.Repeat("a", 40), BaseBranch: "main",
	}
	tests := map[string]struct {
		mutate     func(*AdmissionRequest)
		profile    core.SandboxProfile
		profileErr error
	}{
		"malformed":          {mutate: func(r *AdmissionRequest) { r.Repository = "not a repository" }},
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
			installations := &admissionServiceInstallations{err: errors.New("external authority called")}
			_, _, err := NewAdmissionService(store, &admissionServiceScheduler{}, provider, installations).Admit(context.Background(), request)
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
