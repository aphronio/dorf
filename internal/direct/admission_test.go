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
	session      core.Session
	profile      core.SandboxProfile
	profileErr   error
	conflictOnce bool
	created      bool
	admitted     core.SessionAdmission
}

func (s *admissionServiceStore) SessionExists(context.Context, string) (bool, error) {
	return s.exists, nil
}

func (s *admissionServiceStore) Session(context.Context, string) (core.Session, error) {
	return s.session, nil
}

func (s *admissionServiceStore) ActiveSandboxProfile(context.Context, string) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) DefaultSandboxProfile(context.Context) (core.SandboxProfile, error) {
	return s.profile, s.profileErr
}

func (s *admissionServiceStore) AdmitDirect(_ context.Context, input core.SessionAdmission, _ string) (core.Session, bool, error) {
	s.session.CurrentTaskID = "task-retained"
	s.admitted = input
	if s.conflictOnce {
		s.conflictOnce = false
		return core.Session{}, false, ErrAdmissionConflict
	}
	expected := core.SessionAdmission{
		AdmissionKey: s.session.AdmissionKey, AgentsMD: s.session.AgentsMD, SandboxProfile: s.session.SandboxProfile,
		ProviderConnection: s.session.ProviderConnection, Model: s.session.Model, ReasoningEffort: s.session.ReasoningEffort,
	}
	if s.session.AdmissionKey != "" && input != expected {
		return core.Session{}, false, ErrAdmissionConflict
	}
	return s.session, s.created, nil
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
	session := core.Session{
		ID: "job-direct-request", AdmissionKey: "direct-request", AgentsMD: "preserve exact goal",
		SandboxProfile: "cloud", ProviderConnection: "primary", Model: "gpt-5.6-sol",
		ReasoningEffort: "high", AdmissionOpen: true,
	}
	store := &admissionServiceStore{
		session: session, profile: verifiedAdmissionProfile("cloud"), conflictOnce: true,
	}
	request := AdmissionRequest{AdmissionKey: "direct-request", AgentsMD: session.AgentsMD}

	got, created, err := NewAdmissionService(
		store, "test-queue", &admissionServiceProvider{},
	).Admit(context.Background(), request)
	if err != nil || created || got.ID != session.ID || got.CurrentTaskID == "" {
		t.Fatalf("create race replay Session=%#v created=%t err=%v", got, created, err)
	}
}

func TestAdmissionServiceReplaySkipsVolatileAuthority(t *testing.T) {
	authorityErr := errors.New("volatile authority must be skipped")
	session := core.Session{
		ID: "job-replay", AdmissionKey: "replay", AgentsMD: "goal", SandboxProfile: "cloud",
		ProviderConnection: "primary", Model: "model", ReasoningEffort: "high", AdmissionOpen: true,
	}
	store := &admissionServiceStore{exists: true, session: session, profileErr: authorityErr}
	provider := &admissionServiceProvider{defaultErr: authorityErr, defaultModelErr: authorityErr, checkErr: authorityErr}
	request := AdmissionRequest{AdmissionKey: "replay", AgentsMD: "goal"}

	got, created, err := NewAdmissionService(store, "test-queue", provider).Admit(context.Background(), request)
	if err != nil || created || got.ID != session.ID {
		t.Fatalf("replay Session=%#v created=%t err=%v", got, created, err)
	}
}

func TestAdmissionServiceRejectsConflictingReplay(t *testing.T) {
	request := AdmissionRequest{AdmissionKey: "replay", AgentsMD: "goal", Model: "model"}
	session := core.Session{
		ID: "job-replay", AdmissionKey: "replay", AgentsMD: "goal", SandboxProfile: "cloud",
		ProviderConnection: "primary", Model: "model", ReasoningEffort: "high", AdmissionOpen: true,
	}
	tests := map[string]struct {
		request  AdmissionRequest
		workflow core.WorkflowName
	}{
		"changed input":    {request: AdmissionRequest{AdmissionKey: "replay", AgentsMD: "different", Model: "model"}},
		"malformed replay": {request: AdmissionRequest{AdmissionKey: "replay", AgentsMD: "goal", Model: "model\x00"}},
		"foreign workflow": {request: request, workflow: "coding-to-proposal"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stored := session
			stored.Workflow = test.workflow
			store := &admissionServiceStore{exists: true, session: stored}
			if _, _, err := NewAdmissionService(store, "test-queue", &admissionServiceProvider{}).
				Admit(context.Background(), test.request); !errors.Is(err, ErrAdmissionConflict) {
				t.Fatalf("error=%v, want admission conflict", err)
			}
		})
	}
}

func TestAdmissionServiceValidatesBeforeMutableAuthority(t *testing.T) {
	authorityErr := errors.New("mutable authority must be skipped")
	tests := map[string]AdmissionRequest{
		"missing key":          {AgentsMD: "goal", Model: "model"},
		"invalid instructions": {AdmissionKey: "request", AgentsMD: "\x00", Model: "model"},
		"bad reasoning":        {AdmissionKey: "request", AgentsMD: "goal", Model: "model", ReasoningEffort: "maximum"},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			store := &admissionServiceStore{profileErr: authorityErr}
			provider := &admissionServiceProvider{defaultErr: authorityErr, checkErr: authorityErr}
			_, _, err := NewAdmissionService(store, "test-queue", provider).Admit(context.Background(), request)
			if !errors.Is(err, ErrInvalidAdmission) {
				t.Fatalf("error=%v, want invalid admission", err)
			}
		})
	}
}

func TestAdmissionServiceExplicitConnectionUsesItsDefaultModel(t *testing.T) {
	defaultErr := errors.New("DefaultConnection must be skipped")
	session := core.Session{ID: "job-explicit", AdmissionOpen: true}
	store := &admissionServiceStore{session: session, profile: verifiedAdmissionProfile("explicit-profile"), created: true}
	provider := &admissionServiceProvider{defaultErr: defaultErr}
	request := AdmissionRequest{
		AdmissionKey: "explicit", AgentsMD: "goal", SandboxProfile: " explicit-profile ",
		ProviderConnection: " explicit-connection ",
	}

	_, created, err := NewAdmissionService(store, "test-queue", provider).Admit(context.Background(), request)
	if err != nil || !created || provider.checked != "explicit-connection" ||
		provider.defaultModelConnection != "explicit-connection" || provider.defaultModelCalls != 1 ||
		store.admitted.SandboxProfile != "explicit-profile" || store.admitted.Model != "gpt-5.6-sol" ||
		store.admitted.ReasoningEffort != "high" {
		t.Fatalf("created=%t checked=%q default_model=%q/%d admitted=%#v err=%v", created, provider.checked, provider.defaultModelConnection, provider.defaultModelCalls, store.admitted, err)
	}
}

func TestAdmissionServiceExplicitModelBypassesConnectionDefault(t *testing.T) {
	session := core.Session{ID: "job-explicit-model", AdmissionOpen: true}
	store := &admissionServiceStore{session: session, profile: verifiedAdmissionProfile("profile"), created: true}
	provider := &admissionServiceProvider{defaultModelErr: errors.New("DefaultModel must be skipped")}
	request := AdmissionRequest{AdmissionKey: "explicit-model", AgentsMD: "goal", Model: " model "}

	_, created, err := NewAdmissionService(store, "test-queue", provider).Admit(context.Background(), request)
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
