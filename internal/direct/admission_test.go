package direct

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

type admissionServiceStore struct {
	job          core.Job
	profile      core.SandboxProfile
	profileErr   error
	admitted     core.JobAdmission
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

func (s *admissionServiceStore) SandboxProfile(_ context.Context, _ string) (core.SandboxProfile, error) {
	s.profileCalls++
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) DefaultSandboxProfile(_ context.Context) (core.SandboxProfile, error) {
	s.profileCalls++
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) AdmitDirect(_ context.Context, input core.JobAdmission) (core.Job, bool, error) {
	s.admitCalls++
	s.admitted = input
	if s.conflictOnce {
		s.conflictOnce = false
		return core.Job{}, false, ErrAdmissionConflict
	}
	created := s.job.ID == ""
	if created {
		s.job = core.Job{
			ID: core.JobID(input.AdmissionKey), AdmissionKey: input.AdmissionKey, Goal: input.Goal,
			SandboxProfile: input.SandboxProfile, ProviderConnection: input.ProviderConnection,
			Model: input.Model, ReasoningEffort: input.ReasoningEffort, AdmissionOpen: true,
		}
	} else {
		expected := core.JobAdmission{
			AdmissionKey: s.job.AdmissionKey, Goal: s.job.Goal, SandboxProfile: s.job.SandboxProfile,
			ProviderConnection: s.job.ProviderConnection, Model: s.job.Model, ReasoningEffort: s.job.ReasoningEffort,
		}
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
		return core.Job{}, errors.New("wrong direct task identity")
	}
	job.CurrentTaskID = "task-retained"
	return job, nil
}

type admissionServiceProvider struct {
	defaultCalls int
	checkCalls   int
	checked      string
	err          error
}

func (p *admissionServiceProvider) DefaultConnection() (string, error) {
	p.defaultCalls++
	return "primary", p.err
}

func (p *admissionServiceProvider) Check(_ context.Context, connection string) error {
	p.checkCalls++
	p.checked = connection
	return p.err
}

func TestAdmissionServiceResolvesAuthorityOnlyForNewJobAndReconcilesReplay(t *testing.T) {
	store := &admissionServiceStore{profile: verifiedAdmissionProfile("cloud")}
	scheduler := &admissionServiceScheduler{}
	provider := &admissionServiceProvider{}
	service := NewAdmissionService(store, scheduler, provider)
	request := AdmissionRequest{
		AdmissionKey: "direct-request", Goal: "preserve exact goal", Model: "gpt-5.6-sol",
	}

	createdJob, created, err := service.Admit(context.Background(), request)
	if err != nil || !created || createdJob.CurrentTaskID == "" {
		t.Fatalf("new admission Job=%#v created=%t err=%v", createdJob, created, err)
	}
	if store.admitted.SandboxProfile != "cloud" || store.admitted.ProviderConnection != "primary" {
		t.Fatalf("resolved admission=%#v", store.admitted)
	}

	store.hideJobOnce, store.conflictOnce = true, true
	store.profile = verifiedAdmissionProfile("changed-default")
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
	changed.Goal = "different goal"
	if _, _, err := service.Admit(context.Background(), changed); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("changed replay error=%v", err)
	}
	malformed := request
	malformed.Model = ""
	if _, _, err := service.Admit(context.Background(), malformed); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("malformed replay error=%v", err)
	}
	store.job.Workflow = "coding-to-proposal"
	store.job.WorkflowRevision = "1"
	if _, _, err := service.Admit(context.Background(), request); !errors.Is(err, ErrAdmissionConflict) {
		t.Fatalf("foreign-kind replay error=%v", err)
	}
}

func TestAdmissionServiceValidatesBeforeMutableAuthority(t *testing.T) {
	tests := map[string]AdmissionRequest{
		"missing key":   {Goal: "goal", Model: "model"},
		"blank goal":    {AdmissionKey: "request", Goal: "  ", Model: "model"},
		"missing model": {AdmissionKey: "request", Goal: "goal"},
		"bad reasoning": {AdmissionKey: "request", Goal: "goal", Model: "model", ReasoningEffort: "maximum"},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			store := &admissionServiceStore{profile: verifiedAdmissionProfile("cloud")}
			provider := &admissionServiceProvider{}
			_, _, err := NewAdmissionService(store, &admissionServiceScheduler{}, provider).Admit(context.Background(), request)
			if !errors.Is(err, ErrInvalidAdmission) {
				t.Fatalf("error=%v, want invalid admission", err)
			}
			if store.profileCalls != 0 || provider.defaultCalls != 0 || provider.checkCalls != 0 || store.admitCalls != 0 {
				t.Fatalf("invalid admission crossed mutable authority: profile=%d default=%d check=%d store=%d",
					store.profileCalls, provider.defaultCalls, provider.checkCalls, store.admitCalls)
			}
		})
	}
}

func TestAdmissionServiceHonorsExplicitProfileAndConnection(t *testing.T) {
	store := &admissionServiceStore{profile: verifiedAdmissionProfile("explicit-profile")}
	provider := &admissionServiceProvider{}
	request := AdmissionRequest{
		AdmissionKey: "explicit", Goal: "goal", SandboxProfile: " explicit-profile ",
		ProviderConnection: " explicit-connection ", Model: " model ",
	}
	if _, created, err := NewAdmissionService(store, &admissionServiceScheduler{}, provider).Admit(context.Background(), request); err != nil || !created {
		t.Fatalf("explicit admission created=%t err=%v", created, err)
	}
	if provider.defaultCalls != 0 || provider.checkCalls != 1 || provider.checked != "explicit-connection" ||
		store.admitted.SandboxProfile != "explicit-profile" || store.admitted.Model != "model" || store.admitted.ReasoningEffort != "high" {
		t.Fatalf("provider=%#v admitted=%#v", provider, store.admitted)
	}
}

func verifiedAdmissionProfile(name string) core.SandboxProfile {
	now := time.Unix(1, 0)
	profile := core.SandboxProfile{Name: name}
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	profile.Verification = &core.ProfileVerification{
		ContractVersion: core.BaseProfileContract, DefinitionHash: profile.DefinitionHash, ProbeCompletedAt: now, CleanedAt: now,
	}
	return profile
}
