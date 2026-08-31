package coding

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	githubapi "github.com/aphronio/dorf/internal/github"
)

var (
	ErrInvalidAdmission  = errors.New("invalid coding admission")
	ErrAdmissionConflict = errors.New("coding admission key is bound to different input")
)

// AdmissionRequest is the caller-owned coding input. Deployment authority such
// as the resolved profile, provider default, and GitHub installation is added
// only by AdmissionService and retained in Admission.
type AdmissionRequest struct {
	AdmissionKey       string
	Goal               string
	SandboxProfile     string
	ProviderConnection string
	Model              string
	ReasoningEffort    string
	Repository         string
	Revision           string
	Branch             string
	BaseBranch         string
}

// AdmissionStore is the durable authority needed by coding admission.
type AdmissionStore interface {
	JobExists(context.Context, string) (bool, error)
	Job(context.Context, string) (core.Job, error)
	CodingJob(context.Context, string) (Job, error)
	SandboxProfile(context.Context, string) (core.SandboxProfile, error)
	DefaultSandboxProfile(context.Context) (core.SandboxProfile, error)
	AdmitCoding(context.Context, Admission) (core.Job, bool, error)
}

// AdmissionScheduler reconciles the one deterministic coding task attachment.
type AdmissionScheduler interface {
	ScheduleJobTask(context.Context, core.Job, string, string) (core.Job, error)
}

// AdmissionProvider owns deployment AI-connection selection and readiness.
type AdmissionProvider interface {
	DefaultConnection() (string, error)
	DefaultModel(string) (string, error)
	Check(context.Context, string) error
}

// InstallationDiscovery resolves the GitHub App installation for a new Job.
type InstallationDiscovery interface {
	DiscoverInstallation(context.Context, string) (string, error)
}

type AdmissionService struct {
	store         AdmissionStore
	scheduler     AdmissionScheduler
	provider      AdmissionProvider
	installations InstallationDiscovery
}

func NewAdmissionService(store AdmissionStore, scheduler AdmissionScheduler, provider AdmissionProvider, installations InstallationDiscovery) AdmissionService {
	return AdmissionService{store: store, scheduler: scheduler, provider: provider, installations: installations}
}

func (s AdmissionService) Admit(ctx context.Context, request AdmissionRequest) (core.Job, bool, error) {
	key, err := admissionKey(request.AdmissionKey)
	if err != nil {
		return core.Job{}, false, err
	}
	request.AdmissionKey = key
	if s.store == nil {
		return core.Job{}, false, fmt.Errorf("coding admission store is not configured")
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
	if err := requireRemoteGitCapability(profile); err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
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
	if admission.Model == "" {
		admission.Model, err = s.provider.DefaultModel(admission.ProviderConnection)
		if err != nil {
			return core.Job{}, false, err
		}
	}
	if err := s.provider.Check(ctx, admission.ProviderConnection); err != nil {
		return core.Job{}, false, fmt.Errorf("AI connection %q is not ready: %w", admission.ProviderConnection, err)
	}
	if s.installations == nil {
		return core.Job{}, false, fmt.Errorf("GitHub installation discovery is not configured")
	}
	admission.GitHubInstallation, err = s.installations.DiscoverInstallation(ctx, admission.GitHubRepository)
	if err != nil {
		return core.Job{}, false, err
	}
	admission, err = NormalizeAdmission(admission)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("validate resolved coding admission: %w", err)
	}
	return s.persistAndSchedule(ctx, admission)
}

func (s AdmissionService) replay(ctx context.Context, request AdmissionRequest) (core.Job, bool, error) {
	job, err := s.store.Job(ctx, core.JobID(request.AdmissionKey))
	if err != nil {
		return core.Job{}, false, err
	}
	if job.Workflow != Workflow || job.WorkflowRevision != WorkflowRevision {
		return core.Job{}, false, ErrAdmissionConflict
	}
	stored, err := s.store.CodingJob(ctx, job.ID)
	if err != nil {
		return core.Job{}, false, err
	}
	admission, err := normalizeAdmissionRequest(request)
	if err != nil {
		return core.Job{}, false, fmt.Errorf("%w: %v", ErrAdmissionConflict, err)
	}
	if admission.SandboxProfile != "" && admission.SandboxProfile != stored.SandboxProfile ||
		admission.ProviderConnection != "" && admission.ProviderConnection != stored.ProviderConnection {
		return core.Job{}, false, ErrAdmissionConflict
	}
	admission.SandboxProfile = stored.SandboxProfile
	admission.ProviderConnection = stored.ProviderConnection
	if admission.Model == "" {
		admission.Model = stored.Model
	}
	admission.GitHubInstallation = stored.GitHubInstallation
	admission, err = NormalizeAdmission(admission)
	if err != nil {
		return core.Job{}, false, err
	}
	return s.persistAndSchedule(ctx, admission)
}

func (s AdmissionService) persistAndSchedule(ctx context.Context, admission Admission) (core.Job, bool, error) {
	job, created, err := s.store.AdmitCoding(ctx, admission)
	if err != nil || !job.AdmissionOpen {
		return job, created, err
	}
	if s.scheduler == nil {
		return core.Job{}, created, fmt.Errorf("coding admission scheduling is not configured")
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

func normalizeAdmissionRequest(request AdmissionRequest) (Admission, error) {
	request.AdmissionKey = strings.TrimSpace(request.AdmissionKey)
	request.SandboxProfile = strings.TrimSpace(request.SandboxProfile)
	request.ProviderConnection = strings.TrimSpace(request.ProviderConnection)
	request.Model = strings.TrimSpace(request.Model)
	request.ReasoningEffort = strings.TrimSpace(request.ReasoningEffort)
	request.Repository = strings.TrimSpace(request.Repository)
	request.Revision = strings.TrimSpace(request.Revision)
	request.Branch = strings.TrimSpace(request.Branch)
	request.BaseBranch = strings.TrimSpace(request.BaseBranch)
	if request.ReasoningEffort == "" {
		request.ReasoningEffort = "high"
	}
	if request.Branch == "" {
		request.Branch = "dorf/" + core.JobID(request.AdmissionKey)
	}
	if invalidAdmissionText(request.Goal, 1<<20, true) || invalidAdmissionText(request.Model, 1024, false) ||
		invalidAdmissionText(request.SandboxProfile, 255, false) || invalidAdmissionText(request.ProviderConnection, 255, false) ||
		invalidAdmissionText(request.Repository, 4096, true) || invalidAdmissionText(request.Revision, 64, true) ||
		invalidAdmissionText(request.Branch, 1024, true) || invalidAdmissionText(request.BaseBranch, 1024, true) ||
		(request.ReasoningEffort != "low" && request.ReasoningEffort != "medium" && request.ReasoningEffort != "high" && request.ReasoningEffort != "xhigh") {
		return Admission{}, ErrInvalidAdmission
	}
	repository, err := githubapi.RepositoryFromCloneURL(request.Repository)
	if err != nil {
		return Admission{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
	}
	if !exactCommitOID.MatchString(request.Revision) {
		return Admission{}, fmt.Errorf("%w: admitted revision must be a lowercase full commit OID (40 hex for SHA-1 or 64 hex for SHA-256)", ErrInvalidAdmission)
	}
	if err := githubapi.ValidateRepositoryAuthority(request.Repository, repository, request.BaseBranch, request.Branch); err != nil {
		return Admission{}, fmt.Errorf("%w: %v", ErrInvalidAdmission, err)
	}
	return Admission{
		JobAdmission: core.JobAdmission{
			AdmissionKey: request.AdmissionKey, Workflow: Workflow, WorkflowRevision: WorkflowRevision,
			Goal: request.Goal, SandboxProfile: request.SandboxProfile,
			ProviderConnection: request.ProviderConnection, Model: request.Model, ReasoningEffort: request.ReasoningEffort,
		},
		Repository: request.Repository, Revision: request.Revision, Branch: request.Branch,
		GitHubRepository: repository, BaseBranch: request.BaseBranch,
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

func requireRemoteGitCapability(profile core.SandboxProfile) error {
	if profile.Provider == core.SandboxProviderE2B && !profile.E2BAllowInternet {
		return fmt.Errorf("Sandbox profile %q blocks internet access and cannot use a remote Git source; update and reverify the profile with internet access", profile.Name)
	}
	return nil
}
