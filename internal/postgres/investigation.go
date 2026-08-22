package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/investigation"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) CodebaseInvestigationSource(ctx context.Context, jobID string) (investigation.Source, error) {
	row, err := dbsql.New(s.DB).GetCodebaseInvestigationSource(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return investigation.Source{}, ErrNotFound
	}
	if err != nil {
		return investigation.Source{}, err
	}
	return investigationSourceFromValues(row.JobID, row.Kind, row.Repository, row.Revision, row.BundleDigest, row.BundleByteSize), nil
}

// authorizeInvestigationMessage implements the investigation workflow's
// follow-up policy inside the same locked transaction that records the Message.
func authorizeInvestigationMessage(ctx context.Context, queries *dbsql.Queries, job dbsql.GetJobAdmissionForUpdateRow, input core.MessageAdmission) (admittedAgentRun, error) {
	if input.Intent != core.MessageFollow {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation accepts follow-up Messages only after a draft")
	}
	if input.SandboxID != core.MainSandboxName(input.JobID) {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation Message requires the workflow-authorized default Sandbox")
	}
	latest, err := queries.GetLatestInvestigationRunAndDraft(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	if latest.State != core.AgentRunCompleted || latest.DraftAgentRunID == "" || latest.Harness == "" || latest.ThreadID == "" {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation accepts a follow-up only while waiting on its latest retained draft")
	}
	source, err := queries.GetCodebaseInvestigationSource(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	return admittedAgentRun{
		Role: "investigate", Capability: "repository-read-report", InputRevision: source.Revision,
		SandboxID: input.SandboxID, Harness: latest.Harness, ThreadID: latest.ThreadID,
	}, nil
}

func (s Store) CodebaseInvestigationDrafts(ctx context.Context, jobID string) ([]investigation.Draft, error) {
	rows, err := dbsql.New(s.DB).ListCodebaseInvestigationDrafts(ctx, jobID)
	if err != nil {
		return nil, err
	}
	drafts := make([]investigation.Draft, 0, len(rows))
	for _, row := range rows {
		drafts = append(drafts, investigation.Draft{JobID: row.JobID, MessageID: row.MessageID, AgentRunID: row.AgentRunID, Content: row.Content, CreatedAt: timeValue(row.FinishedAt).UTC()})
	}
	return drafts, nil
}

func (s Store) CodebaseInvestigationMessages(ctx context.Context, jobID string) ([]investigation.MessageRecord, error) {
	rows, err := dbsql.New(s.DB).ListCodebaseInvestigationMessages(ctx, jobID)
	if err != nil {
		return nil, err
	}
	work := make([]investigation.MessageRecord, 0, len(rows))
	for _, row := range rows {
		work = append(work, investigation.MessageRecord{MessageID: row.MessageID, SandboxID: row.SandboxID, Outcome: agentRunOutcome(row.State, row.TurnOutcome), Attention: row.Attention})
	}
	return work, nil
}

func (s Store) ValidateInvestigationAgentMessage(ctx context.Context, execution core.AgentMessageExecution) error {
	source, err := s.CodebaseInvestigationSource(ctx, execution.Job.ID)
	if err != nil {
		return err
	}
	if execution.AgentRun.Role != "investigate" || execution.AgentRun.Capability != "repository-read-report" ||
		execution.AgentRun.InputRevision != source.Revision || execution.AgentRun.SandboxID != core.MainSandboxName(execution.Job.ID) {
		return fmt.Errorf("Message %s conflicts with the exact investigation Agent contract", execution.Message.ID)
	}
	messages, err := s.CodebaseInvestigationMessages(ctx, execution.Job.ID)
	if err != nil {
		return err
	}
	drafts, err := s.CodebaseInvestigationDrafts(ctx, execution.Job.ID)
	if err != nil {
		return err
	}
	drafted := make(map[string]struct{}, len(drafts))
	for _, draft := range drafts {
		drafted[draft.MessageID] = struct{}{}
	}
	for _, message := range messages {
		if _, ok := drafted[message.MessageID]; ok {
			continue
		}
		if message.MessageID != execution.Message.ID || message.SandboxID != execution.Sandbox.ID {
			return fmt.Errorf("Message %s is no longer the exact eligible investigation Agent Message", execution.Message.ID)
		}
		return nil
	}
	return fmt.Errorf("Message %s is no longer eligible for investigation Agent reconciliation", execution.Message.ID)
}

func (s Store) RecordCodebaseInvestigationDraft(ctx context.Context, messageID, content string) (investigation.Draft, bool, error) {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(content) == "" {
		return investigation.Draft{}, false, fmt.Errorf("codebase-investigation draft requires a Message and nonblank Markdown")
	}
	queries := dbsql.New(s.DB)
	message, err := queries.GetMessage(ctx, messageID)
	if err != nil {
		return investigation.Draft{}, false, err
	}
	runRow, err := queries.GetAgentRunByMessage(ctx, messageID)
	if err != nil {
		return investigation.Draft{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return investigation.Draft{}, false, err
	}
	defer tx.Rollback()
	queries = dbsql.New(s.DB).WithTx(tx)
	run, err := queries.GetCodebaseInvestigationRunForUpdate(ctx, dbsql.GetCodebaseInvestigationRunForUpdateParams{JobID: message.JobID, AgentRunID: runRow.ID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return investigation.Draft{}, false, ErrNotFound
		}
		return investigation.Draft{}, false, err
	}
	if run.WorkflowName != investigation.Workflow || run.WorkflowRevision != investigation.WorkflowRevision || run.Role != "investigate" ||
		run.State != core.AgentRunCompleted || run.TurnID == "" || run.TurnOutcome != "completed" || run.InputRevision != run.Revision ||
		!run.StartedAt.Valid || !run.FinishedAt.Valid {
		return investigation.Draft{}, false, fmt.Errorf("investigation draft conflicts with its completed exact-Revision AgentRun")
	}
	drafts, err := queries.ListCodebaseInvestigationDrafts(ctx, message.JobID)
	if err != nil {
		return investigation.Draft{}, false, err
	}
	for _, row := range drafts {
		if row.AgentRunID != runRow.ID {
			continue
		}
		existing := investigation.Draft{JobID: row.JobID, MessageID: row.MessageID, AgentRunID: row.AgentRunID, Content: row.Content, CreatedAt: timeValue(row.FinishedAt).UTC()}
		if existing.Content != content {
			return investigation.Draft{}, false, fmt.Errorf("AgentRun %s already has a different immutable investigation draft", runRow.ID)
		}
		return existing, false, nil
	}
	if !run.AdmissionOpen || run.CleanupState != core.CleanupPending {
		return investigation.Draft{}, false, fmt.Errorf("investigation draft cannot be recorded after admission closes or cleanup begins")
	}
	createdAt := run.FinishedAt.Time.UTC()
	if err := expectOneRows(queries.InsertCodebaseInvestigationDraft(ctx, dbsql.InsertCodebaseInvestigationDraftParams{
		JobID: message.JobID, AgentRunID: runRow.ID, Content: content,
	})); err != nil {
		return investigation.Draft{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return investigation.Draft{}, false, err
	}
	return investigation.Draft{JobID: message.JobID, MessageID: messageID, AgentRunID: runRow.ID, Content: content, CreatedAt: createdAt}, true, nil
}
