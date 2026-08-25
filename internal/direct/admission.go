package direct

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/profile"
)

const DirectAgentRole = "direct"

type ProviderChecker interface {
	Check(context.Context, string) error
}

type AdmissionStore interface {
	JobExists(context.Context, string) (bool, error)
	AdmitDirect(context.Context, core.JobAdmission) (core.Job, bool, error)
}

// Admit bootstraps one direct client Job and reconciles its durable task
// attachment. Follow, steer, Thread reuse, and AgentRun recovery remain Core
// Message mechanics after this initial admission.
func Admit(ctx context.Context, store AdmissionStore, application core.Application, providers ProviderChecker, runtime profile.Runtime, input core.JobAdmission) (core.Job, bool, error) {
	input.AdmissionKey = strings.TrimSpace(input.AdmissionKey)
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	if input.Workflow != "" || input.WorkflowRevision != "" {
		return core.Job{}, false, fmt.Errorf("direct admission cannot use workflow identity")
	}
	if input.AdmissionKey == "" {
		return core.Job{}, false, fmt.Errorf("direct admission requires a stable admission key")
	}
	exists, err := store.JobExists(ctx, core.JobID(input.AdmissionKey))
	if err != nil {
		return core.Job{}, false, err
	}
	if !exists {
		if strings.TrimSpace(runtime.SandboxProfile) == "" || runtime.SandboxProfile != strings.TrimSpace(input.SandboxProfile) {
			return core.Job{}, false, fmt.Errorf("direct Job requires its exact named Sandbox profile")
		}
		if providers == nil {
			return core.Job{}, false, fmt.Errorf("provider readiness is not configured")
		}
		if err := providers.Check(ctx, strings.TrimSpace(input.ProviderConnection)); err != nil {
			return core.Job{}, false, fmt.Errorf("AI connection %q is not ready: %w", strings.TrimSpace(input.ProviderConnection), err)
		}
	}
	job, created, err := store.AdmitDirect(ctx, input)
	if err != nil || !job.AdmissionOpen {
		return job, created, err
	}
	job, err = application.ScheduleJobTask(ctx, job, TaskName, TaskKey(job.ID))
	return job, created, err
}
