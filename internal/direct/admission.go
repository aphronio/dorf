package direct

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gateway"
	profileapp "github.com/aphronio/dorf/internal/profile"
)

const DirectAgentRole = "direct"

var (
	ErrInvalidAdmission  = errors.New("invalid direct admission")
	ErrAdmissionConflict = errors.New("direct admission key is bound to different input")
)

// AdmissionRequest is the caller-owned direct input. Deployment defaults are
// resolved only for a new Job and retained in its durable admission.
type AdmissionRequest struct {
	AdmissionKey       string
	Goal               string
	SandboxProfile     string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
}

// AdmissionStore is the durable authority needed by direct admission.
type AdmissionStore interface {
	JobExists(context.Context, string) (bool, error)
	Job(context.Context, string) (core.Job, error)
	profileapp.SelectionStore
	AdmitDirect(context.Context, core.JobAdmission, string) (core.Job, bool, error)
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

// Admit bootstraps one direct client Job and reconciles its durable task
// attachment. Follow, steer, Thread reuse, and AgentRun recovery remain Core
// Message mechanics after this initial admission.
func (s AdmissionService) Admit(ctx context.Context, request AdmissionRequest) (core.Job, bool, error) {
	key, err := admissionKey(request.AdmissionKey)
	if err != nil {
		return core.Job{}, false, err
	}
	request.AdmissionKey = key
	if s.store == nil {
		return core.Job{}, false, fmt.Errorf("direct admission store is not configured")
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
	admission.SandboxProfile = profile.Name
	admission.ProviderConnection, admission.Model, err = gateway.ResolveModel(s.provider, admission.ProviderConnection, admission.Model)
	if err != nil {
		return core.Job{}, false, err
	}
	admission.Model = strings.TrimSpace(admission.Model)
	if invalidAdmissionText(admission.Model, 1024, true) {
		return core.Job{}, false, fmt.Errorf("%w: AI connection returned invalid default model", ErrInvalidAdmission)
	}
	if err := s.provider.Check(ctx, admission.ProviderConnection); err != nil {
		return core.Job{}, false, fmt.Errorf("AI connection %q is not ready: %w", admission.ProviderConnection, err)
	}
	return s.store.AdmitDirect(ctx, admission, s.queueName)
}

func (s AdmissionService) replay(ctx context.Context, request AdmissionRequest) (core.Job, bool, error) {
	job, err := s.store.Job(ctx, core.JobID(request.AdmissionKey))
	if err != nil {
		return core.Job{}, false, err
	}
	if job.Workflow != "" || job.WorkflowRevision != "" {
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
	return s.store.AdmitDirect(ctx, admission, s.queueName)
}

func normalizeAdmissionRequest(request AdmissionRequest) (core.JobAdmission, error) {
	request.SandboxProfile = strings.TrimSpace(request.SandboxProfile)
	request.ProviderConnection = strings.TrimSpace(request.ProviderConnection)
	request.Model = strings.TrimSpace(request.Model)
	request.ReasoningEffort = strings.TrimSpace(request.ReasoningEffort)
	if request.ReasoningEffort == "" {
		request.ReasoningEffort = "high"
	}
	if invalidAdmissionText(request.Goal, 1<<20, true) || invalidAdmissionText(request.Model, 1024, false) ||
		invalidAdmissionText(request.SandboxProfile, 255, false) || invalidAdmissionText(request.ProviderConnection, 255, false) ||
		(request.ReasoningEffort != "low" && request.ReasoningEffort != "medium" && request.ReasoningEffort != "high" && request.ReasoningEffort != "xhigh") {
		return core.JobAdmission{}, ErrInvalidAdmission
	}
	return core.JobAdmission{
		AdmissionKey: request.AdmissionKey, Goal: request.Goal, SandboxProfile: request.SandboxProfile,
		ProviderConnection: request.ProviderConnection, Model: request.Model, ReasoningEffort: request.ReasoningEffort,
	}, nil
}

func admissionKey(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || value != trimmed || len(trimmed) > 255 || strings.ContainsRune(trimmed, 0) {
		return "", ErrInvalidAdmission
	}
	return trimmed, nil
}

func invalidAdmissionText(value string, limit int, required bool) bool {
	return len(value) > limit || strings.ContainsRune(value, 0) || required && strings.TrimSpace(value) == ""
}
