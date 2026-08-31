package direct

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

type admissionServiceStore struct {
	exists       bool
	job          core.Job
	profile      core.SandboxProfile
	profileErr   error
	conflictOnce bool
	created      bool
	admitted     core.JobAdmission
}

func (s *admissionServiceStore) JobExists(context.Context, string) (bool, error) {
	return s.exists, nil
}

func (s *admissionServiceStore) Job(context.Context, string) (core.Job, error) { return s.job, nil }

func (s *admissionServiceStore) SandboxProfile(context.Context, string) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) DefaultSandboxProfile(context.Context) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) AdmitDirect(_ context.Context, input core.JobAdmission) (core.Job, bool, error) {
	s.admitted = input
	if s.conflictOnce {
		s.conflictOnce = false
		return core.Job{}, false, ErrAdmissionConflict
	}
	expected := core.JobAdmission{
		AdmissionKey: s.job.AdmissionKey, Goal: s.job.Goal, SandboxProfile: s.job.SandboxProfile,
		ProviderConnection: s.job.ProviderConnection, Model: s.job.Model, ReasoningEffort: s.job.ReasoningEffort,
	}
	if s.job.AdmissionKey != "" && input != expected {
		return core.Job{}, false, ErrAdmissionConflict
	}
	return s.job, s.created, nil
}

type admissionServiceScheduler struct{}

func (admissionServiceScheduler) ScheduleJobTask(_ context.Context, job core.Job, taskName, taskKey string) (core.Job, error) {
	if taskName != TaskName || taskKey != TaskKey(job.ID) {
		return core.Job{}, errors.New("wrong direct task identity")
	}
	job.CurrentTaskID = "task-retained"
	return job, nil
}

type admissionServiceProvider struct {
	defaultErr             error
	defaultModelErr        error
	defaultModel           string
	defaultModelConnection string
	defaultModelCalls      int
	checkErr               error
	checked                string
}

func (p *admissionServiceProvider) DefaultConnection() (string, error) {
	return "primary", p.defaultErr
}

func (p *admissionServiceProvider) DefaultModel(connection string) (string, error) {
	p.defaultModelCalls++
	p.defaultModelConnection = connection
	if p.defaultModel == "" {
		p.defaultModel = "gpt-5.6-sol"
	}
	return p.defaultModel, p.defaultModelErr
}

func (p *admissionServiceProvider) Check(_ context.Context, connection string) error {
	p.checked = connection
	return p.checkErr
}

func TestAdmissionServiceCreateRaceReplaysDurableAdmission(t *testing.T) {
	job := core.Job{
		ID: "job-direct-request", AdmissionKey: "direct-request", Goal: "preserve exact goal",
		SandboxProfile: "cloud", ProviderConnection: "primary", Model: "gpt-5.6-sol",
		ReasoningEffort: "high", AdmissionOpen: true,
	}
	store := &admissionServiceStore{
		job: job, profile: verifiedAdmissionProfile("cloud"), conflictOnce: true,
	}
	request := AdmissionRequest{AdmissionKey: "direct-request", Goal: job.Goal}

	got, created, err := NewAdmissionService(
		store, admissionServiceScheduler{}, &admissionServiceProvider{},
	).Admit(context.Background(), request)
	if err != nil || created || got.ID != job.ID || got.CurrentTaskID == "" {
		t.Fatalf("create race replay Job=%#v created=%t err=%v", got, created, err)
	}
}

func TestAdmissionServiceReplaySkipsVolatileAuthority(t *testing.T) {
	authorityErr := errors.New("volatile authority must be skipped")
	job := core.Job{
		ID: "job-replay", AdmissionKey: "replay", Goal: "goal", SandboxProfile: "cloud",
		ProviderConnection: "primary", Model: "model", ReasoningEffort: "high", AdmissionOpen: true,
	}
	store := &admissionServiceStore{exists: true, job: job, profileErr: authorityErr}
	provider := &admissionServiceProvider{defaultErr: authorityErr, defaultModelErr: authorityErr, checkErr: authorityErr}
	request := AdmissionRequest{AdmissionKey: "replay", Goal: "goal"}

	got, created, err := NewAdmissionService(store, admissionServiceScheduler{}, provider).Admit(context.Background(), request)
	if err != nil || created || got.ID != job.ID {
		t.Fatalf("replay Job=%#v created=%t err=%v", got, created, err)
	}
}

func TestAdmissionServiceRejectsConflictingReplay(t *testing.T) {
	request := AdmissionRequest{AdmissionKey: "replay", Goal: "goal", Model: "model"}
	job := core.Job{
		ID: "job-replay", AdmissionKey: "replay", Goal: "goal", SandboxProfile: "cloud",
		ProviderConnection: "primary", Model: "model", ReasoningEffort: "high", AdmissionOpen: true,
	}
	tests := map[string]struct {
		request  AdmissionRequest
		workflow core.WorkflowName
	}{
		"changed input":    {request: AdmissionRequest{AdmissionKey: "replay", Goal: "different", Model: "model"}},
		"malformed replay": {request: AdmissionRequest{AdmissionKey: "replay", Goal: "goal", Model: "model\x00"}},
		"foreign workflow": {request: request, workflow: "coding-to-proposal"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stored := job
			stored.Workflow = test.workflow
			store := &admissionServiceStore{exists: true, job: stored}
			if _, _, err := NewAdmissionService(store, admissionServiceScheduler{}, &admissionServiceProvider{}).
				Admit(context.Background(), test.request); !errors.Is(err, ErrAdmissionConflict) {
				t.Fatalf("error=%v, want admission conflict", err)
			}
		})
	}
}

func TestAdmissionServiceValidatesBeforeMutableAuthority(t *testing.T) {
	authorityErr := errors.New("mutable authority must be skipped")
	tests := map[string]AdmissionRequest{
		"missing key":   {Goal: "goal", Model: "model"},
		"blank goal":    {AdmissionKey: "request", Goal: "  ", Model: "model"},
		"bad reasoning": {AdmissionKey: "request", Goal: "goal", Model: "model", ReasoningEffort: "maximum"},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			store := &admissionServiceStore{profileErr: authorityErr}
			provider := &admissionServiceProvider{defaultErr: authorityErr, checkErr: authorityErr}
			_, _, err := NewAdmissionService(store, admissionServiceScheduler{}, provider).Admit(context.Background(), request)
			if !errors.Is(err, ErrInvalidAdmission) {
				t.Fatalf("error=%v, want invalid admission", err)
			}
		})
	}
}

func TestAdmissionServiceExplicitConnectionUsesItsDefaultModel(t *testing.T) {
	defaultErr := errors.New("DefaultConnection must be skipped")
	job := core.Job{ID: "job-explicit", AdmissionOpen: true}
	store := &admissionServiceStore{job: job, profile: verifiedAdmissionProfile("explicit-profile"), created: true}
	provider := &admissionServiceProvider{defaultErr: defaultErr}
	request := AdmissionRequest{
		AdmissionKey: "explicit", Goal: "goal", SandboxProfile: " explicit-profile ",
		ProviderConnection: " explicit-connection ",
	}

	_, created, err := NewAdmissionService(store, admissionServiceScheduler{}, provider).Admit(context.Background(), request)
	if err != nil || !created || provider.checked != "explicit-connection" ||
		provider.defaultModelConnection != "explicit-connection" || provider.defaultModelCalls != 1 ||
		store.admitted.SandboxProfile != "explicit-profile" || store.admitted.Model != "gpt-5.6-sol" ||
		store.admitted.ReasoningEffort != "high" {
		t.Fatalf("created=%t checked=%q default_model=%q/%d admitted=%#v err=%v", created, provider.checked, provider.defaultModelConnection, provider.defaultModelCalls, store.admitted, err)
	}
}

func TestAdmissionServiceExplicitModelBypassesConnectionDefault(t *testing.T) {
	job := core.Job{ID: "job-explicit-model", AdmissionOpen: true}
	store := &admissionServiceStore{job: job, profile: verifiedAdmissionProfile("profile"), created: true}
	provider := &admissionServiceProvider{defaultModelErr: errors.New("DefaultModel must be skipped")}
	request := AdmissionRequest{AdmissionKey: "explicit-model", Goal: "goal", Model: " model "}

	_, created, err := NewAdmissionService(store, admissionServiceScheduler{}, provider).Admit(context.Background(), request)
	if err != nil || !created || provider.defaultModelCalls != 0 || store.admitted.Model != "model" {
		t.Fatalf("created=%t default_model_calls=%d admitted=%#v err=%v", created, provider.defaultModelCalls, store.admitted, err)
	}
}

func verifiedAdmissionProfile(name string) core.SandboxProfile {
	now := time.Unix(1, 0)
	profile := core.SandboxProfile{Name: name}
	profile.DefinitionHash = profile.CurrentDefinitionHash()
	profile.Verification = &core.ProfileVerification{
		ContractVersion: core.BaseProfileContract, DefinitionHash: profile.DefinitionHash,
		ProbeCompletedAt: now, CleanedAt: now,
	}
	return profile
}
