package investigation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gateway"
	profileapp "github.com/aphronio/dorf/internal/profile"
)

var (
	ErrInvalidAdmission  = errors.New("invalid codebase-investigation admission")
	ErrAdmissionConflict = errors.New("codebase-investigation admission key is bound to different input")
)

// AdmissionRequest is the caller-owned investigation input. The deployment's
// selected profile and provider authority are resolved only by AdmissionService.
type AdmissionRequest struct {
	AdmissionKey       string
	Brief              string
	SandboxProfile     string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
	Source             Source
}

// AdmissionStore is the durable authority needed by investigation admission.
type AdmissionStore interface {
	JobExists(context.Context, string) (bool, error)
	Job(context.Context, string) (core.Job, error)
	profileapp.SelectionStore
	AdmitInvestigation(context.Context, Admission, string) (core.Job, bool, error)
}

// AdmissionProvider owns deployment AI-connection selection and readiness.
type AdmissionProvider interface {
	gateway.ModelDefaults
	Check(context.Context, string) error
}

type AdmissionService struct {
	store     AdmissionStore
	queueName string
	provider  AdmissionProvider
}

func NewAdmissionService(store AdmissionStore, queueName string, provider AdmissionProvider) AdmissionService {
	return AdmissionService{store: store, queueName: queueName, provider: provider}
}

func (s AdmissionService) Admit(ctx context.Context, request AdmissionRequest) (core.Job, bool, error) {
	key, err := investigationAdmissionKey(request.AdmissionKey)
	if err != nil {
		return core.Job{}, false, err
	}
	request.AdmissionKey = key
	if s.store == nil {
		return core.Job{}, false, fmt.Errorf("investigation admission store is not configured")
	}
	exists, err := s.store.JobExists(ctx, core.JobID(key))
	if err != nil {
		return core.Job{}, false, err
	}
	if exists {
		return s.replay(ctx, request)
	}
	job, created, err := s.admitNew(ctx, request)
	if errors.Is(err, ErrAdmissionConflict) {
		replayed, _, replayErr := s.replay(ctx, request)
		return replayed, created, replayErr
	}
	return job, created, err
}

func (s AdmissionService) admitNew(ctx context.Context, request AdmissionRequest) (core.Job, bool, error) {
	admission, err := normalizeAdmissionRequest(request)
	if err != nil {
		return core.Job{}, false, err
	}
	profile, err := profileapp.SelectVerified(ctx, s.store, admission.SandboxProfile)
	if err != nil {
		if admission.SandboxProfile != "" {
			return core.Job{}, false, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
		}
		return core.Job{}, false, err
	}
	if err := requireRemoteGitCapability(profile); err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
	}
	admission.SandboxProfile = profile.Name
	admission.ProviderConnection, admission.Model, err = gateway.ResolveModel(s.provider, admission.ProviderConnection, admission.Model)
	if err != nil {
		return core.Job{}, false, err
	}
	if err := s.provider.Check(ctx, admission.ProviderConnection); err != nil {
		return core.Job{}, false, fmt.Errorf("AI connection %q is not ready: %w", admission.ProviderConnection, err)
	}
	return s.store.AdmitInvestigation(ctx, admission, s.queueName)
}

func (s AdmissionService) replay(ctx context.Context, request AdmissionRequest) (core.Job, bool, error) {
	job, err := s.store.Job(ctx, core.JobID(request.AdmissionKey))
	if err != nil {
		return core.Job{}, false, err
	}
	if job.Workflow != Workflow || job.WorkflowRevision != WorkflowRevision {
		return core.Job{}, false, ErrAdmissionConflict
	}
	admission, err := normalizeAdmissionRequest(request)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", ErrAdmissionConflict, err)
	}
	if admission.SandboxProfile != "" && admission.SandboxProfile != job.SandboxProfile ||
		admission.ProviderConnection != "" && admission.ProviderConnection != job.ProviderConnection {
		return core.Job{}, false, ErrAdmissionConflict
	}
	admission.SandboxProfile = job.SandboxProfile
	admission.ProviderConnection = job.ProviderConnection
	if admission.Model == "" {
		admission.Model = job.Model
	}
	return s.store.AdmitInvestigation(ctx, admission, s.queueName)
}

func normalizeAdmissionRequest(request AdmissionRequest) (Admission, error) {
	request.AdmissionKey = strings.TrimSpace(request.AdmissionKey)
	request.SandboxProfile = strings.TrimSpace(request.SandboxProfile)
	request.ProviderConnection = strings.TrimSpace(request.ProviderConnection)
	request.Model = strings.TrimSpace(request.Model)
	request.ReasoningEffort = strings.TrimSpace(request.ReasoningEffort)
	if request.ReasoningEffort == "" {
		request.ReasoningEffort = "high"
	}
	if invalidAdmissionText(request.Brief, 1<<20, true) || invalidAdmissionText(request.Model, 1024, false) ||
		invalidAdmissionText(request.SandboxProfile, 255, false) || invalidAdmissionText(request.ProviderConnection, 255, false) ||
		invalidAdmissionText(request.Source.Repository, 4096, true) ||
		(request.ReasoningEffort != "low" && request.ReasoningEffort != "medium" && request.ReasoningEffort != "high" && request.ReasoningEffort != "xhigh") {
		return Admission{}, ErrInvalidAdmission
	}
	admission := Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: request.AdmissionKey, Goal: request.Brief, SandboxProfile: request.SandboxProfile,
			ProviderConnection: request.ProviderConnection, Model: request.Model, ReasoningEffort: request.ReasoningEffort,
		},
		Source: request.Source,
	}
	validated, err := NormalizeAdmission(admission)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
	}
	return validated, nil
}

func investigationAdmissionKey(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || value != trimmed || len(trimmed) > 255 || strings.ContainsRune(trimmed, 0) {
		return "", ErrInvalidAdmission
	}
	return trimmed, nil
}

func invalidAdmissionText(value string, limit int, required bool) bool {
	return len(value) > limit || strings.ContainsRune(value, 0) || required && strings.TrimSpace(value) == ""
}

func requireRemoteGitCapability(profile core.SandboxProfile) error {
	if profile.Provider == core.SandboxProviderE2B && !profile.E2BAllowInternet {
		return fmt.Errorf("Sandbox profile %q blocks internet access and cannot use a remote Git source; update and reverify the profile with internet access", profile.Name)
	}
	return nil
}
