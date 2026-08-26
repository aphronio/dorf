package direct

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
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
	SandboxProfile(context.Context, string) (core.SandboxProfile, error)
	DefaultSandboxProfile(context.Context) (core.SandboxProfile, error)
	AdmitDirect(context.Context, core.JobAdmission) (core.Job, bool, error)
}

// AdmissionScheduler reconciles the one deterministic direct task attachment.
type AdmissionScheduler interface {
	ScheduleJobTask(context.Context, core.Job, string, string) (core.Job, error)
}

// AdmissionProvider owns deployment AI-connection selection and readiness.
type AdmissionProvider interface {
	DefaultConnection() (string, error)
	Check(context.Context, string) error
}

// AdmissionService is the single direct admission authority used by every
// local and remote adapter in a deployment.
type AdmissionService struct {
	store     AdmissionStore
	scheduler AdmissionScheduler
	provider  AdmissionProvider
}

func NewAdmissionService(store AdmissionStore, scheduler AdmissionScheduler, provider AdmissionProvider) AdmissionService {
	return AdmissionService{store: store, scheduler: scheduler, provider: provider}
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
	profile, err := s.selectProfile(ctx, admission.SandboxProfile)
	if err != nil {
		return core.Job{}, false, err
	}
	admission.SandboxProfile = profile.Name
	if s.provider == nil {
		return core.Job{}, false, fmt.Errorf("provider readiness is not configured")
	}
	if admission.ProviderConnection == "" {
		admission.ProviderConnection, err = s.provider.DefaultConnection()
		if err != nil {
			return core.Job{}, false, err
		}
	}
	if err := s.provider.Check(ctx, admission.ProviderConnection); err != nil {
		return core.Job{}, false, fmt.Errorf("AI connection %q is not ready: %w", admission.ProviderConnection, err)
	}
	return s.persistAndSchedule(ctx, admission)
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
	return s.persistAndSchedule(ctx, admission)
}

func (s AdmissionService) persistAndSchedule(ctx context.Context, admission core.JobAdmission) (core.Job, bool, error) {
	job, created, err := s.store.AdmitDirect(ctx, admission)
	if err != nil || !job.AdmissionOpen {
		return job, created, err
	}
	if s.scheduler == nil {
		return core.Job{}, created, fmt.Errorf("direct admission scheduling is not configured")
	}
	job, err = s.scheduler.ScheduleJobTask(ctx, job, TaskName, TaskKey(job.ID))
	return job, created, err
}

func (s AdmissionService) selectProfile(ctx context.Context, requested string) (core.SandboxProfile, error) {
	var (
		profile core.SandboxProfile
		err     error
	)
	if requested == "" {
		profile, err = s.store.DefaultSandboxProfile(ctx)
	} else {
		profile, err = s.store.SandboxProfile(ctx, requested)
	}
	if err != nil {
		if requested != "" {
			return core.SandboxProfile{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
		}
		return core.SandboxProfile{}, err
	}
	if !profile.BaseVerified() {
		err := fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", profile.Name, core.BaseProfileContract)
		if requested != "" {
			return core.SandboxProfile{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
		}
		return core.SandboxProfile{}, err
	}
	return profile, nil
}

func normalizeAdmissionRequest(request AdmissionRequest) (core.JobAdmission, error) {
	request.SandboxProfile = strings.TrimSpace(request.SandboxProfile)
	request.ProviderConnection = strings.TrimSpace(request.ProviderConnection)
	request.Model = strings.TrimSpace(request.Model)
	request.ReasoningEffort = strings.TrimSpace(request.ReasoningEffort)
	if request.ReasoningEffort == "" {
		request.ReasoningEffort = "high"
	}
	if invalidAdmissionText(request.Goal, 1<<20, true) || invalidAdmissionText(request.Model, 1024, true) ||
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
