package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
)

type InvestigationWorkKind string

const (
	InvestigationWorkComplete     InvestigationWorkKind = "complete"
	InvestigationWorkAttention    InvestigationWorkKind = "attention"
	InvestigationWorkAction       InvestigationWorkKind = "action"
	InvestigationWorkDeliver      InvestigationWorkKind = "deliver-message"
	InvestigationWorkObserveAgent InvestigationWorkKind = "observe-agent-run"
	InvestigationWorkRecordReport InvestigationWorkKind = "record-report"
)

type InvestigationWork struct {
	Kind       InvestigationWorkKind `json:"kind"`
	FactID     string                `json:"fact_id,omitempty"`
	ActionKind spine.ActionKind      `json:"action,omitempty"`
	Scope      string                `json:"scope,omitempty"`
	Detail     string                `json:"detail,omitempty"`
}

type InvestigationSnapshot struct {
	Job         spine.Job
	MainSandbox spine.Sandbox
	Actions     []spine.Action
	Delivery    spine.Delivery
	Report      *spine.CodebaseInvestigationReport
}

func LoadCodebaseInvestigation(ctx context.Context, store postgres.Store, jobID string) (InvestigationSnapshot, error) {
	var snapshot InvestigationSnapshot
	var err error
	snapshot.Job, err = store.Job(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Job.Workflow != spine.WorkflowCodebaseInvestigation || snapshot.Job.WorkflowRevision != spine.CodebaseInvestigationRevision {
		return snapshot, fmt.Errorf("Job %s is not codebase-investigation revision %s", jobID, spine.CodebaseInvestigationRevision)
	}
	sandboxes, err := store.Sandboxes(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	for _, owned := range sandboxes {
		if owned.ID == spine.MainSandboxName(jobID) {
			snapshot.MainSandbox = owned
			break
		}
	}
	if snapshot.MainSandbox.ID == "" {
		return snapshot, fmt.Errorf("main Sandbox %s: %w", spine.MainSandboxName(jobID), postgres.ErrNotFound)
	}
	snapshot.Actions, err = store.Actions(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	deliveries, err := store.Deliveries(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	for _, delivery := range deliveries {
		if delivery.AgentRun.Role != "investigate" {
			continue
		}
		if snapshot.Delivery.AgentRun.ID != "" {
			return snapshot, fmt.Errorf("codebase-investigation Job has multiple investigator AgentRuns")
		}
		snapshot.Delivery = delivery
	}
	if snapshot.Delivery.AgentRun.ID == "" || snapshot.Delivery.AgentRun.SandboxID != snapshot.MainSandbox.ID {
		return snapshot, fmt.Errorf("codebase-investigation Job has no exact main-Sandbox investigator AgentRun")
	}
	snapshot.Report, err = store.CodebaseInvestigationReport(ctx, jobID)
	return snapshot, err
}

func (s InvestigationSnapshot) Project() InvestigationWork {
	run := s.Delivery.AgentRun
	if s.Report != nil {
		return InvestigationWork{Kind: InvestigationWorkComplete, FactID: s.Report.JobID, Detail: "report recorded"}
	}
	if !s.Job.AdmissionOpen {
		return InvestigationWork{Kind: InvestigationWorkComplete, FactID: s.Job.ID, Detail: "admission closed"}
	}
	action := func(kind spine.ActionKind) InvestigationWork {
		return InvestigationWork{Kind: InvestigationWorkAction, FactID: spine.ScopedActionID(s.Job.ID, kind, s.MainSandbox.ID), ActionKind: kind, Scope: s.MainSandbox.ID}
	}
	if !actionSucceeded(s.Actions, spine.ActionSandboxCreate, s.MainSandbox.ID) {
		return action(spine.ActionSandboxCreate)
	}
	if !actionSucceeded(s.Actions, spine.ActionRepositoryClone, s.MainSandbox.ID) {
		return action(spine.ActionRepositoryClone)
	}
	if !actionSucceeded(s.Actions, spine.ActionRouteCreate, s.MainSandbox.ID) {
		return action(spine.ActionRouteCreate)
	}
	switch run.State {
	case spine.AgentRunPending, spine.AgentRunSubmitting:
		return InvestigationWork{Kind: InvestigationWorkDeliver, FactID: run.ID}
	case spine.AgentRunActive:
		return InvestigationWork{Kind: InvestigationWorkObserveAgent, FactID: run.ID}
	case spine.AgentRunCompleted:
		if s.Job.WorkflowAttentionSource == run.ID && s.Job.WorkflowAttention != "" {
			return InvestigationWork{Kind: InvestigationWorkAttention, FactID: run.ID, Detail: s.Job.WorkflowAttention}
		}
		return InvestigationWork{Kind: InvestigationWorkRecordReport, FactID: run.ID}
	case spine.AgentRunFailed, spine.AgentRunInterrupted, spine.AgentRunUncertain:
		detail := run.Attention
		if detail == "" {
			detail = string(run.State)
		}
		return InvestigationWork{Kind: InvestigationWorkAttention, FactID: run.ID, Detail: detail}
	default:
		return InvestigationWork{Kind: InvestigationWorkAttention, FactID: run.ID, Detail: "unsupported investigator AgentRun state " + string(run.State)}
	}
}

func RunCodebaseInvestigation(ctx context.Context, service spine.Service, store postgres.Store, jobID string) (InvestigationWork, error) {
	for {
		snapshot, err := LoadCodebaseInvestigation(ctx, store, jobID)
		if err != nil {
			return InvestigationWork{}, err
		}
		work := snapshot.Project()
		switch work.Kind {
		case InvestigationWorkComplete, InvestigationWorkAttention:
			return work, nil
		case InvestigationWorkAction:
			err = runInvestigationAction(ctx, service, store, snapshot, work)
		case InvestigationWorkDeliver:
			err = runFactStep(ctx, agentRunStepName(work.FactID), work.FactID, func(workCtx context.Context) error {
				return service.Deliver(workCtx, snapshot.Job, snapshot.Delivery)
			})
		case InvestigationWorkObserveAgent:
			turn, observeErr := service.ObserveAgentRunTurn(ctx, snapshot.Job, snapshot.Delivery.AgentRun, "investigate")
			if observeErr != nil {
				return InvestigationWork{}, observeErr
			}
			if turn.Status == "completed" || turn.Status == "failed" || turn.Status == "interrupted" {
				continue
			}
			return work, nil
		case InvestigationWorkRecordReport:
			err = runFactStep(ctx, "dorf/investigation-report/v1/"+work.FactID, work.FactID, func(workCtx context.Context) error {
				return recordInvestigationReport(workCtx, service, store, snapshot)
			})
		default:
			return InvestigationWork{}, fmt.Errorf("unsupported codebase-investigation work %q", work.Kind)
		}
		if err != nil {
			var contractErr investigationContractError
			if errors.As(err, &contractErr) {
				if attentionErr := store.SetWorkflowAttention(ctx, snapshot.Job.ID, work.FactID, contractErr.Error()); attentionErr != nil {
					return InvestigationWork{}, errors.Join(err, attentionErr)
				}
				return InvestigationWork{Kind: InvestigationWorkAttention, FactID: work.FactID, Detail: contractErr.Error()}, nil
			}
			return InvestigationWork{}, err
		}
	}
}

func runInvestigationAction(ctx context.Context, service spine.Service, store postgres.Store, snapshot InvestigationSnapshot, work InvestigationWork) error {
	if work.Scope != snapshot.MainSandbox.ID || work.FactID != spine.ScopedActionID(snapshot.Job.ID, work.ActionKind, work.Scope) {
		return fmt.Errorf("investigation Action does not match the exact main Sandbox")
	}
	action, err := store.GetOrCreateSandboxAction(ctx, work.Scope, work.ActionKind)
	if err != nil {
		return err
	}
	if action.ID != work.FactID || action.JobID != snapshot.Job.ID || action.Scope != work.Scope || action.Kind != work.ActionKind {
		return fmt.Errorf("selected investigation Action changed identity")
	}
	if action.State == spine.ActionSucceeded {
		return nil
	}
	return runActionStep(ctx, action.ID, func(workCtx context.Context) error {
		return service.ExecuteSandboxAction(workCtx, snapshot.Job, snapshot.MainSandbox, action)
	})
}

type investigationContractError string

func (e investigationContractError) Error() string { return string(e) }

func recordInvestigationReport(ctx context.Context, service spine.Service, store postgres.Store, snapshot InvestigationSnapshot) error {
	run := snapshot.Delivery.AgentRun
	turn, err := service.ObserveAgentRunTurn(ctx, snapshot.Job, run, "investigate")
	if err != nil {
		return err
	}
	if turn.Status != "completed" || turn.ID != run.TurnID {
		return investigationContractError("investigator did not return an exact completed Harness Turn")
	}
	report, err := validateInvestigationReport(turn.Output)
	if err != nil {
		return err
	}
	if err := service.VerifyRepositoryUnchanged(ctx, snapshot.Job); err != nil {
		return investigationContractError("investigator changed or dirtied the admitted checkout: " + err.Error())
	}
	blob, err := service.EvidenceStore().Put([]byte(report))
	if err != nil {
		return err
	}
	evidence := spine.Evidence{
		ID: spine.EvidenceID(run.ID, "investigation-report"), Digest: blob.Digest, ByteSize: blob.ByteSize,
		MediaType: "text/markdown", Producer: "dorf-codebase-investigation", Kind: "investigation-report",
		AgentRunID: run.ID, Revision: snapshot.Job.Revision, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
	}
	receipt := spine.CodebaseInvestigationReport{
		JobID: snapshot.Job.ID, AgentRunID: run.ID,
		ReportEvidenceID: evidence.ID, ObservedAt: run.FinishedAt,
	}
	_, _, err = store.RecordCodebaseInvestigationReport(ctx, receipt, evidence)
	return err
}

func validateInvestigationReport(output string) (string, error) {
	if len(output) > 1<<20 {
		return "", investigationContractError("investigation report exceeds 1 MiB")
	}
	output = strings.ReplaceAll(output, "\r\n", "\n")
	if strings.TrimSpace(output) == "" {
		return "", investigationContractError("investigation report must be nonblank Markdown")
	}
	return strings.TrimSpace(output) + "\n", nil
}
