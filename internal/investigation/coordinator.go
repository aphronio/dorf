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

type WorkKind string

const (
	WorkComplete    WorkKind = "complete"
	WorkAttention   WorkKind = "attention"
	WorkAction      WorkKind = "action"
	WorkWaitAgent   WorkKind = "wait-agent"
	WorkRecordDraft WorkKind = "record-draft"
	WorkWaitInput   WorkKind = "wait-client-input"
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
	if w.Kind == WorkWaitAgent {
		return "Awaiting investigator result"
	}
	return definition.OperationLabel(string(w.Kind), humanizeIdentifier(string(w.Kind)))
}

type Snapshot struct {
	Job         core.Job
	MainSandbox core.Sandbox
	Actions     []core.Action
	Messages    []MessageRecord
	Message     MessageRecord
	Drafts      []Draft
	Source      Source
}

// MessageRecord is investigation's read-only projection of an admitted
// investigator Message. Core owns its hidden AgentRun and Harness lifecycle.
type MessageRecord struct {
	MessageID string
	SandboxID string
	Outcome   string
	Attention string
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
	snapshot.Messages, err = store.CodebaseInvestigationMessages(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	if len(snapshot.Messages) == 0 {
		return snapshot, fmt.Errorf("codebase-investigation Job has no exact investigator Message")
	}
	for _, message := range snapshot.Messages {
		if message.MessageID == "" || message.SandboxID != snapshot.MainSandbox.ID {
			return snapshot, fmt.Errorf("codebase-investigation Message does not use the exact main Sandbox")
		}
	}
	snapshot.Drafts, err = store.CodebaseInvestigationDrafts(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	drafted := make(map[string]struct{}, len(snapshot.Drafts))
	for _, draft := range snapshot.Drafts {
		drafted[draft.MessageID] = struct{}{}
	}
	for _, message := range snapshot.Messages {
		if _, ok := drafted[message.MessageID]; !ok {
			snapshot.Message = message
			break
		}
	}
	if snapshot.Message.MessageID == "" {
		snapshot.Message = snapshot.Messages[len(snapshot.Messages)-1]
	}
	return snapshot, nil
}

func (s Snapshot) Project() Work {
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
		if draft.MessageID == s.Message.MessageID {
			return Work{Kind: WorkWaitInput, FactID: draft.ArtifactID, Detail: "send a follow-up or request cleanup"}
		}
	}
	if s.Job.WorkflowAttentionSource == s.Message.MessageID && s.Job.WorkflowAttention != "" {
		return Work{Kind: WorkAttention, FactID: s.Message.MessageID, Detail: s.Job.WorkflowAttention}
	}
	if s.Message.Attention != "" || s.Message.Outcome != "" && s.Message.Outcome != "completed" {
		return Work{Kind: WorkAttention, FactID: s.Message.MessageID, Detail: messageAttention(s.Message)}
	}
	if s.Message.Outcome == "completed" {
		return Work{Kind: WorkRecordDraft, FactID: s.Message.MessageID}
	}
	return Work{Kind: WorkWaitAgent, FactID: s.Message.MessageID, Detail: "awaiting investigator result"}
}

func messageAttention(message MessageRecord) string {
	if message.Attention != "" {
		return message.Attention
	}
	return "investigator Harness work ended with outcome " + message.Outcome
}

// SelectAgentMessage is investigation's static eligibility policy. Core calls
// it under the Job fence before the typed coordinator observes workflow facts.
func SelectAgentMessage(ctx context.Context, store Store, jobID string) (*core.AgentMessageWork, error) {
	snapshot, err := LoadSnapshot(ctx, store, jobID)
	if err != nil {
		return nil, err
	}
	if !snapshot.Job.AdmissionOpen ||
		!investigationActionSucceeded(snapshot.Actions, core.ActionSandboxCreate, snapshot.MainSandbox.ID) ||
		!investigationActionSucceeded(snapshot.Actions, core.ActionRouteCreate, snapshot.MainSandbox.ID) {
		return nil, nil
	}
	materialization := gitworkspace.ActionRepositoryClone
	if snapshot.Source.Kind == SourceGitBundle {
		materialization = ActionRepositoryRestore
	}
	if !investigationActionSucceeded(snapshot.Actions, materialization, snapshot.MainSandbox.ID) ||
		snapshot.Message.Outcome != "" || snapshot.Message.Attention != "" {
		return nil, nil
	}
	for _, draft := range snapshot.Drafts {
		if draft.MessageID == snapshot.Message.MessageID {
			return nil, nil
		}
	}
	if snapshot.Message.MessageID == "" || snapshot.Message.SandboxID != snapshot.MainSandbox.ID {
		return nil, nil
	}
	return &core.AgentMessageWork{MessageID: snapshot.Message.MessageID, SandboxID: snapshot.Message.SandboxID}, nil
}

func Run(ctx context.Context, custody core.JobHandle, service Service, store Store, jobID string) (Work, error) {
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
			err = runInvestigationAction(ctx, custody, service, store, snapshot, work)
		case WorkRecordDraft:
			err = absurdruntime.RunFactStep(ctx, "dorf/investigation-draft/v2/"+work.FactID, work.FactID, func(workCtx context.Context) error {
				return recordInvestigationDraft(workCtx, service, store, snapshot)
			})
		case WorkWaitAgent:
			return work, nil
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

// AgentPrompt is investigation-owned prompt policy selected by the static
// deployment composition from authoritative durable facts.
func AgentPrompt(source Source, input string) string {
	return fmt.Sprintf(`%s

Dorf codebase-investigation contract:
- Inspect the repository at exact Revision %s; the current working directory is its root.
- Do not modify the checkout.
- Return a nonblank Markdown report grounded in repository-relative paths with 1-based line numbers, formatted as <path>:<line> or <path>:<start>-<end>.
- Do not include absolute Sandbox paths.
- If there is no useful finding, say that plainly in the report.`, strings.TrimSpace(input), source.Revision)
}

func runInvestigationAction(ctx context.Context, custody core.JobHandle, service Service, store Store, snapshot Snapshot, work Work) error {
	if work.Scope != snapshot.MainSandbox.ID || work.FactID != core.ScopedActionID(snapshot.Job.ID, work.ActionKind, work.Scope) {
		return fmt.Errorf("investigation Action does not match the exact main Sandbox")
	}
	// Sandbox-create stays visible to inspection while its provider mutation is
	// executed only through Core's opaque Job custody handle.
	if work.ActionKind == core.ActionSandboxCreate {
		ensured, err := custody.EnsureDefaultSandbox(ctx)
		if err != nil {
			return err
		}
		if ensured.ID() != snapshot.MainSandbox.ID {
			return fmt.Errorf("ensured Sandbox %s changed selected identity %s", ensured.ID(), snapshot.MainSandbox.ID)
		}
		return nil
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
		return service.ExecuteSandboxAction(workCtx, snapshot.Job.ID, action.ID)
	})
}

type investigationContractError string

func (e investigationContractError) Error() string { return string(e) }

func recordInvestigationDraft(ctx context.Context, service Service, store Store, snapshot Snapshot) error {
	result, err := service.ObserveSettledAgentMessage(ctx, snapshot.Job.ID, snapshot.Message.MessageID)
	if err != nil {
		return err
	}
	if result.MessageID != snapshot.Message.MessageID || result.Outcome != "completed" {
		return investigationContractError("investigator did not return an exact completed Harness Turn")
	}
	report, err := validateInvestigationReport(result.Output)
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
	artifact := core.Artifact{
		JobID:  snapshot.Job.ID,
		Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "text/markdown",
		Producer: "dorf-codebase-investigation",
	}
	_, _, err = store.RecordCodebaseInvestigationDraft(ctx, result.MessageID, artifact)
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
