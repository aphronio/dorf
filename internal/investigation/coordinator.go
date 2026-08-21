package investigation

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

func investigationAgentRunStepName(id string) string { return "dorf/agent-run/v1/" + id }

type WorkKind string

const (
	WorkComplete     WorkKind = "complete"
	WorkAttention    WorkKind = "attention"
	WorkAction       WorkKind = "action"
	WorkDeliver      WorkKind = "deliver-message"
	WorkObserveAgent WorkKind = "observe-agent-run"
	WorkRecordDraft  WorkKind = "record-draft"
	WorkWaitInput    WorkKind = "wait-client-input"
)

type Work struct {
	Kind       WorkKind        `json:"kind"`
	FactID     string          `json:"fact_id,omitempty"`
	ActionKind core.ActionKind `json:"action,omitempty"`
	Scope      string          `json:"scope,omitempty"`
	Detail     string          `json:"detail,omitempty"`
}

func (w Work) Description() string {
	definition := WorkflowDefinition()
	if w.Kind == WorkAction {
		return definition.ActionLabel(w.ActionKind)
	}
	if w.Kind == WorkObserveAgent {
		return definition.AgentRoleLabel("investigate") + " running"
	}
	if w.Kind == WorkDeliver {
		return "Delivering brief to " + lowerFirst(definition.AgentRoleLabel("investigate"))
	}
	if w.Kind == WorkRecordDraft {
		return "Recording " + lowerFirst(definition.ResultLabel("investigation-draft"))
	}
	return definition.OperationLabel(string(w.Kind), humanizeIdentifier(string(w.Kind)))
}

type Snapshot struct {
	Job         core.Job
	MainSandbox core.Sandbox
	Actions     []core.Action
	Deliveries  []core.Delivery
	Delivery    core.Delivery
	Drafts      []Draft
	Source      Source
}

func LoadSnapshot(ctx context.Context, store Store, jobID string) (Snapshot, error) {
	var snapshot Snapshot
	var err error
	snapshot.Job, err = store.Job(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Job.Workflow != Workflow || snapshot.Job.WorkflowRevision != WorkflowRevision {
		return snapshot, fmt.Errorf("Job %s is not codebase-investigation revision %s", jobID, WorkflowRevision)
	}
	snapshot.Source, err = store.CodebaseInvestigationSource(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Source.JobID != snapshot.Job.ID {
		return snapshot, fmt.Errorf("codebase-investigation source conflicts with its exact Job")
	}
	sandboxes, err := store.Sandboxes(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	for _, owned := range sandboxes {
		if owned.ID == core.MainSandboxName(jobID) {
			snapshot.MainSandbox = owned
			break
		}
	}
	if snapshot.MainSandbox.ID == "" {
		return snapshot, fmt.Errorf("main Sandbox %s is not durably reserved", core.MainSandboxName(jobID))
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
		if delivery.AgentRun.Role == "investigate" {
			snapshot.Deliveries = append(snapshot.Deliveries, delivery)
		}
	}
	if len(snapshot.Deliveries) == 0 {
		return snapshot, fmt.Errorf("codebase-investigation Job has no exact main-Sandbox investigator AgentRun")
	}
	for _, delivery := range snapshot.Deliveries {
		if delivery.AgentRun.SandboxID != snapshot.MainSandbox.ID {
			return snapshot, fmt.Errorf("codebase-investigation AgentRun does not use the exact main Sandbox")
		}
	}
	snapshot.Drafts, err = store.CodebaseInvestigationDrafts(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	drafted := make(map[string]struct{}, len(snapshot.Drafts))
	for _, draft := range snapshot.Drafts {
		drafted[draft.AgentRunID] = struct{}{}
	}
	for _, delivery := range snapshot.Deliveries {
		if _, ok := drafted[delivery.AgentRun.ID]; !ok {
			snapshot.Delivery = delivery
			break
		}
	}
	if snapshot.Delivery.AgentRun.ID == "" {
		snapshot.Delivery = snapshot.Deliveries[len(snapshot.Deliveries)-1]
	}
	return snapshot, nil
}

func (s Snapshot) Project() Work {
	run := s.Delivery.AgentRun
	if !s.Job.AdmissionOpen {
		return Work{Kind: WorkComplete, FactID: s.Job.ID, Detail: "admission closed"}
	}
	action := func(kind core.ActionKind) Work {
		work := Work{Kind: WorkAction, FactID: core.ScopedActionID(s.Job.ID, kind, s.MainSandbox.ID), ActionKind: kind, Scope: s.MainSandbox.ID}
		if s.Job.WorkflowAttentionSource == work.FactID && s.Job.WorkflowAttention != "" {
			work.Kind = WorkAttention
			work.Detail = s.Job.WorkflowAttention
		}
		return work
	}
	if !investigationActionSucceeded(s.Actions, core.ActionSandboxCreate, s.MainSandbox.ID) {
		return action(core.ActionSandboxCreate)
	}
	materializationAction := gitworkspace.ActionRepositoryClone
	if s.Source.Kind == SourceGitBundle {
		materializationAction = ActionRepositoryRestore
	}
	if !investigationActionSucceeded(s.Actions, materializationAction, s.MainSandbox.ID) {
		return action(materializationAction)
	}
	if !investigationActionSucceeded(s.Actions, core.ActionRouteCreate, s.MainSandbox.ID) {
		return action(core.ActionRouteCreate)
	}
	for _, draft := range s.Drafts {
		if draft.AgentRunID == run.ID {
			return Work{Kind: WorkWaitInput, FactID: draft.ArtifactID, Detail: "send a follow-up or request cleanup"}
		}
	}
	switch run.State {
	case core.AgentRunPending, core.AgentRunSubmitting:
		return Work{Kind: WorkDeliver, FactID: run.ID}
	case core.AgentRunActive:
		return Work{Kind: WorkObserveAgent, FactID: run.ID}
	case core.AgentRunCompleted:
		if s.Job.WorkflowAttentionSource == run.ID && s.Job.WorkflowAttention != "" {
			return Work{Kind: WorkAttention, FactID: run.ID, Detail: s.Job.WorkflowAttention}
		}
		return Work{Kind: WorkRecordDraft, FactID: run.ID}
	case core.AgentRunFailed, core.AgentRunInterrupted, core.AgentRunUncertain:
		detail := run.Attention
		if detail == "" {
			detail = string(run.State)
		}
		return Work{Kind: WorkAttention, FactID: run.ID, Detail: detail}
	default:
		return Work{Kind: WorkAttention, FactID: run.ID, Detail: "unsupported investigator AgentRun state " + string(run.State)}
	}
}

func Run(ctx context.Context, service Service, store Store, jobID string) (Work, error) {
	for {
		snapshot, err := LoadSnapshot(ctx, store, jobID)
		if err != nil {
			return Work{}, err
		}
		work := snapshot.Project()
		switch work.Kind {
		case WorkComplete, WorkAttention, WorkWaitInput:
			return work, nil
		case WorkAction:
			err = runInvestigationAction(ctx, service, store, snapshot, work)
		case WorkDeliver:
			err = absurdruntime.RunFactStep(ctx, investigationAgentRunStepName(work.FactID), work.FactID, func(workCtx context.Context) error {
				return service.Deliver(workCtx, snapshot.Job, snapshot.Delivery, investigationAgentInput(snapshot.Source, snapshot.Delivery))
			})
		case WorkObserveAgent:
			turn, observeErr := service.ObserveAgentRunTurn(ctx, snapshot.Job, snapshot.Delivery.AgentRun, "investigate")
			if observeErr != nil {
				return Work{}, observeErr
			}
			if turn.Status == "completed" || turn.Status == "failed" || turn.Status == "interrupted" {
				continue
			}
			return work, nil
		case WorkRecordDraft:
			err = absurdruntime.RunFactStep(ctx, "dorf/investigation-draft/v2/"+work.FactID, work.FactID, func(workCtx context.Context) error {
				return recordInvestigationDraft(workCtx, service, store, snapshot)
			})
		default:
			return Work{}, fmt.Errorf("unsupported codebase-investigation work %q", work.Kind)
		}
		if err != nil {
			var contractErr investigationContractError
			if errors.As(err, &contractErr) {
				if attentionErr := store.SetWorkflowAttention(ctx, snapshot.Job.ID, work.FactID, contractErr.Error()); attentionErr != nil {
					return Work{}, errors.Join(err, attentionErr)
				}
				return Work{Kind: WorkAttention, FactID: work.FactID, Detail: contractErr.Error()}, nil
			}
			return work, err
		}
	}
}

func investigationAgentInput(source Source, delivery core.Delivery) string {
	return fmt.Sprintf(`%s

Dorf codebase-investigation contract:
- Inspect the repository at exact Revision %s; the current working directory is its root.
- Do not modify the checkout.
- Return a nonblank Markdown report grounded in repository-relative paths with 1-based line numbers, formatted as <path>:<line> or <path>:<start>-<end>.
- Do not include absolute Sandbox paths.
- If there is no useful finding, say that plainly in the report.`, strings.TrimSpace(delivery.Message.Input), source.Revision)
}

func runInvestigationAction(ctx context.Context, service Service, store Store, snapshot Snapshot, work Work) error {
	if work.Scope != snapshot.MainSandbox.ID || work.FactID != core.ScopedActionID(snapshot.Job.ID, work.ActionKind, work.Scope) {
		return fmt.Errorf("investigation Action does not match the exact main Sandbox")
	}
	action, err := store.GetOrCreateSandboxAction(ctx, work.Scope, work.ActionKind)
	if err != nil {
		return err
	}
	if action.ID != work.FactID || action.JobID != snapshot.Job.ID || action.Scope != work.Scope || action.Kind != work.ActionKind {
		return fmt.Errorf("selected investigation Action changed identity")
	}
	if action.State == core.ActionSucceeded {
		return nil
	}
	return absurdruntime.RunActionStep(ctx, action.ID, func(workCtx context.Context) error {
		if work.ActionKind == ActionRepositoryRestore {
			return service.ExecuteRepositoryRestore(workCtx, snapshot.Job, snapshot.MainSandbox, action, snapshot.Source)
		}
		if work.ActionKind == gitworkspace.ActionRepositoryClone {
			return service.ExecuteRepositoryClone(workCtx, snapshot.Job, snapshot.MainSandbox, action, snapshot.Source.Repository, snapshot.Source.Revision, "")
		}
		return service.ExecuteSandboxAction(workCtx, snapshot.Job, snapshot.MainSandbox, action)
	})
}

type investigationContractError string

func (e investigationContractError) Error() string { return string(e) }

func recordInvestigationDraft(ctx context.Context, service Service, store Store, snapshot Snapshot) error {
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
	if err := service.VerifyRepositoryUnchanged(ctx, snapshot.Job, snapshot.Source.Revision); err != nil {
		return investigationContractError("investigator changed or dirtied the admitted checkout: " + err.Error())
	}
	blob, err := service.BlobStore().Put([]byte(report))
	if err != nil {
		return err
	}
	name := DraftArtifactName(snapshot.Delivery.Message.Sequence)
	artifact := core.Artifact{
		ID:    core.ArtifactID(snapshot.Job.ID, name),
		JobID: snapshot.Job.ID, Name: name,
		Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "text/markdown",
		Producer: "dorf-codebase-investigation", AgentRunID: run.ID, CreatedAt: run.FinishedAt,
	}
	_, _, err = store.RecordCodebaseInvestigationDraft(ctx, artifact)
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

func investigationActionSucceeded(actions []core.Action, kind core.ActionKind, scope string) bool {
	for _, action := range actions {
		if action.Kind == kind && action.Scope == scope {
			return action.State == core.ActionSucceeded
		}
	}
	return false
}
