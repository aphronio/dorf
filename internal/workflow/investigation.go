package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/investigation"
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
	InvestigationWorkRecordDraft  InvestigationWorkKind = "record-draft"
	InvestigationWorkWaitInput    InvestigationWorkKind = "wait-client-input"
)

type InvestigationWork struct {
	Kind       InvestigationWorkKind `json:"kind"`
	FactID     string                `json:"fact_id,omitempty"`
	ActionKind spine.ActionKind      `json:"action,omitempty"`
	Scope      string                `json:"scope,omitempty"`
	Detail     string                `json:"detail,omitempty"`
}

func (w InvestigationWork) Description() string {
	definition := CodebaseInvestigationDefinition()
	if w.Kind == InvestigationWorkAction {
		return definition.ActionLabel(w.ActionKind)
	}
	if w.Kind == InvestigationWorkObserveAgent {
		return definition.AgentRoleLabel("investigate") + " running"
	}
	if w.Kind == InvestigationWorkDeliver {
		return "Delivering brief to " + lowerFirst(definition.AgentRoleLabel("investigate"))
	}
	if w.Kind == InvestigationWorkRecordDraft {
		return "Recording " + lowerFirst(definition.ResultLabel("investigation-draft"))
	}
	return definition.OperationLabel(string(w.Kind), humanizeIdentifier(string(w.Kind)))
}

type InvestigationSnapshot struct {
	Job         spine.Job
	MainSandbox spine.Sandbox
	Actions     []spine.Action
	Deliveries  []spine.Delivery
	Delivery    spine.Delivery
	Drafts      []investigation.Draft
	Source      investigation.Source
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

func (s InvestigationSnapshot) Project() InvestigationWork {
	run := s.Delivery.AgentRun
	if !s.Job.AdmissionOpen {
		return InvestigationWork{Kind: InvestigationWorkComplete, FactID: s.Job.ID, Detail: "admission closed"}
	}
	action := func(kind spine.ActionKind) InvestigationWork {
		return InvestigationWork{Kind: InvestigationWorkAction, FactID: spine.ScopedActionID(s.Job.ID, kind, s.MainSandbox.ID), ActionKind: kind, Scope: s.MainSandbox.ID}
	}
	if !actionSucceeded(s.Actions, spine.ActionSandboxCreate, s.MainSandbox.ID) {
		return action(spine.ActionSandboxCreate)
	}
	repositoryAction := spine.ActionRepositoryClone
	if s.Source.Kind == investigation.SourceGitBundle {
		repositoryAction = spine.ActionRepositoryRestore
	}
	if !actionSucceeded(s.Actions, repositoryAction, s.MainSandbox.ID) {
		return action(repositoryAction)
	}
	if !actionSucceeded(s.Actions, spine.ActionRouteCreate, s.MainSandbox.ID) {
		return action(spine.ActionRouteCreate)
	}
	for _, draft := range s.Drafts {
		if draft.AgentRunID == run.ID {
			return InvestigationWork{Kind: InvestigationWorkWaitInput, FactID: draft.ArtifactID, Detail: "send a follow-up or request cleanup"}
		}
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
		return InvestigationWork{Kind: InvestigationWorkRecordDraft, FactID: run.ID}
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

func RunCodebaseInvestigation(ctx context.Context, service investigation.Service, store postgres.Store, jobID string) (InvestigationWork, error) {
	for {
		snapshot, err := LoadCodebaseInvestigation(ctx, store, jobID)
		if err != nil {
			return InvestigationWork{}, err
		}
		work := snapshot.Project()
		switch work.Kind {
		case InvestigationWorkComplete, InvestigationWorkAttention, InvestigationWorkWaitInput:
			return work, nil
		case InvestigationWorkAction:
			err = runInvestigationAction(ctx, service, store, snapshot, work)
		case InvestigationWorkDeliver:
			err = runFactStep(ctx, agentRunStepName(work.FactID), work.FactID, func(workCtx context.Context) error {
				return service.Deliver(workCtx, snapshot.Job, snapshot.Delivery, investigationAgentInput(snapshot.Source, snapshot.Delivery))
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
		case InvestigationWorkRecordDraft:
			err = runFactStep(ctx, "dorf/investigation-draft/v2/"+work.FactID, work.FactID, func(workCtx context.Context) error {
				return recordInvestigationDraft(workCtx, service, store, snapshot)
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

func investigationAgentInput(source investigation.Source, delivery spine.Delivery) string {
	return fmt.Sprintf(`%s

Dorf codebase-investigation contract:
- Inspect the repository at exact Revision %s; the current working directory is its root.
- Do not modify the checkout.
- Return a nonblank Markdown report grounded in repository-relative paths with 1-based line numbers, formatted as <path>:<line> or <path>:<start>-<end>.
- Do not include absolute Sandbox paths.
- If there is no useful finding, say that plainly in the report.`, strings.TrimSpace(delivery.Message.Input), source.Revision)
}

func runInvestigationAction(ctx context.Context, service investigation.Service, store postgres.Store, snapshot InvestigationSnapshot, work InvestigationWork) error {
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
	return absurdruntime.RunActionStep(ctx, action.ID, func(workCtx context.Context) error {
		if work.ActionKind == spine.ActionRepositoryRestore {
			return service.ExecuteRepositoryRestore(workCtx, snapshot.Job, snapshot.MainSandbox, action, snapshot.Source)
		}
		if work.ActionKind == spine.ActionRepositoryClone {
			return service.ExecuteRepositoryClone(workCtx, snapshot.Job, snapshot.MainSandbox, action, snapshot.Source.Repository, snapshot.Source.Revision, "")
		}
		return service.ExecuteSandboxAction(workCtx, snapshot.Job, snapshot.MainSandbox, action)
	})
}

type investigationContractError string

func (e investigationContractError) Error() string { return string(e) }

func recordInvestigationDraft(ctx context.Context, service investigation.Service, store postgres.Store, snapshot InvestigationSnapshot) error {
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
	name := investigation.DraftArtifactName(snapshot.Delivery.Message.Sequence)
	artifact := spine.Artifact{
		ID:    spine.ArtifactID(snapshot.Job.ID, name),
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
