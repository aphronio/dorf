package investigation

import (
	"context"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

type WorkKind string

const (
	WorkComplete  WorkKind = "complete"
	WorkAttention WorkKind = "attention"
	WorkAction    WorkKind = "action"
	WorkWaitAgent WorkKind = "wait-agent"
)

type Work struct {
	Kind       WorkKind        `json:"kind"`
	FactID     string          `json:"fact_id,omitempty"`
	ActionKind core.ActionKind `json:"action,omitempty"`
	Scope      string          `json:"scope,omitempty"`
	Detail     string          `json:"detail,omitempty"`
}

func (w Work) Description() string {
	if w.Kind == "" {
		return "No current workflow operation"
	}
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
	Message     MessageRecord
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
	messages, err := store.CodebaseInvestigationMessages(ctx, jobID)
	if err != nil {
		return snapshot, err
	}
	if len(messages) == 0 {
		return snapshot, fmt.Errorf("codebase-investigation Job has no exact investigator Message")
	}
	for _, message := range messages {
		if message.MessageID == "" || message.SandboxID != snapshot.MainSandbox.ID {
			return snapshot, fmt.Errorf("codebase-investigation Message does not use the exact main Sandbox")
		}
		if snapshot.Message.MessageID == "" && message.Outcome == "" && message.Attention == "" {
			snapshot.Message = message
		}
	}
	if snapshot.Message.MessageID == "" {
		snapshot.Message = messages[len(messages)-1]
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
	if !core.HasSucceededAction(s.Actions, core.ActionSandboxCreate, s.MainSandbox.ID) {
		return action(core.ActionSandboxCreate)
	}
	materializationAction := gitworkspace.ActionRepositoryClone
	if s.Source.Kind == SourceGitBundle {
		materializationAction = ActionRepositoryRestore
	}
	if !core.HasSucceededAction(s.Actions, materializationAction, s.MainSandbox.ID) {
		return action(materializationAction)
	}
	if !core.HasSucceededAction(s.Actions, core.ActionRouteCreate, s.MainSandbox.ID) {
		return action(core.ActionRouteCreate)
	}
	if s.Message.MessageID == "" {
		return Work{}
	}
	if s.Job.WorkflowAttentionSource == s.Message.MessageID && s.Job.WorkflowAttention != "" {
		return Work{Kind: WorkAttention, FactID: s.Message.MessageID, Detail: s.Job.WorkflowAttention}
	}
	if s.Message.Attention != "" || s.Message.Outcome != "" && s.Message.Outcome != "completed" {
		return Work{Kind: WorkAttention, FactID: s.Message.MessageID, Detail: messageAttention(s.Message)}
	}
	if s.Message.Outcome == "completed" {
		return Work{}
	}
	return Work{Kind: WorkWaitAgent, FactID: s.Message.MessageID, Detail: "awaiting investigator result"}
}

func messageAttention(message MessageRecord) string {
	if message.Attention != "" {
		return message.Attention
	}
	return "investigator Harness work ended with outcome " + message.Outcome
}

func Run(ctx context.Context, custody core.JobHandle, service Service, store Store, jobID string) (Work, error) {
	for {
		snapshot, err := LoadSnapshot(ctx, store, jobID)
		if err != nil {
			return Work{}, err
		}
		work := snapshot.Project()
		switch work.Kind {
		case "", WorkComplete, WorkAttention:
			return work, nil
		case WorkAction:
			err = runInvestigationAction(ctx, custody, service, snapshot, work)
		case WorkWaitAgent:
			return work, nil
		default:
			return Work{}, fmt.Errorf("unsupported codebase-investigation work %q", work.Kind)
		}
		if err != nil {
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
- Write the complete Markdown report to %s at the workspace root before finishing. Replace its contents when revising the report.
- Ground the report in repository-relative paths with 1-based line numbers, formatted as <path>:<line> or <path>:<start>-<end>.
- Do not include absolute Sandbox paths.
- If there is no useful finding, say that plainly in the report.`, strings.TrimSpace(input), source.Revision, ReportPath)
}

func runInvestigationAction(ctx context.Context, custody core.JobHandle, service Service, snapshot Snapshot, work Work) error {
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
	if work.ActionKind == ActionRepositoryRestore {
		return service.ExecuteRepositoryRestore(ctx, snapshot.Job, snapshot.MainSandbox, snapshot.Source)
	}
	if work.ActionKind == gitworkspace.ActionRepositoryClone {
		return service.ExecuteRepositoryClone(ctx, snapshot.Job, snapshot.MainSandbox, snapshot.Source.Repository, snapshot.Source.Revision, "")
	}
	return service.ExecuteSandboxAction(ctx, snapshot.Job.ID, snapshot.MainSandbox.ID, work.ActionKind)
}
