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
	latest, err := queries.GetLatestInvestigationRunAndDraft(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	if latest.State != core.AgentRunCompleted || latest.ArtifactID == "" || latest.Harness == "" || latest.ThreadID == "" {
		return admittedAgentRun{}, fmt.Errorf("codebase-investigation accepts a follow-up only while waiting on its latest retained draft")
	}
	source, err := queries.GetCodebaseInvestigationSource(ctx, input.JobID)
	if err != nil {
		return admittedAgentRun{}, err
	}
	return admittedAgentRun{
		Role: "investigate", Capability: "repository-read-report", InputRevision: source.Revision,
		SandboxID: core.MainSandboxName(input.JobID), Harness: latest.Harness, ThreadID: latest.ThreadID,
	}, nil
}

func (s Store) CodebaseInvestigationDrafts(ctx context.Context, jobID string) ([]investigation.Draft, error) {
	rows, err := dbsql.New(s.DB).ListCodebaseInvestigationDrafts(ctx, jobID)
	if err != nil {
		return nil, err
	}
	drafts := make([]investigation.Draft, 0, len(rows))
	for _, row := range rows {
		drafts = append(drafts, investigation.Draft{JobID: row.JobID, AgentRunID: row.AgentRunID, ArtifactID: row.ArtifactID, CreatedAt: row.CreatedAt.UTC()})
	}
	return drafts, nil
}

func (s Store) RecordCodebaseInvestigationDraft(ctx context.Context, artifact core.Artifact) (investigation.Draft, bool, error) {
	if err := validateInvestigationDraft(artifact); err != nil {
		return investigation.Draft{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return investigation.Draft{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	run, err := queries.GetCodebaseInvestigationRunForUpdate(ctx, dbsql.GetCodebaseInvestigationRunForUpdateParams{JobID: artifact.JobID, AgentRunID: artifact.AgentRunID})
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
	drafts, err := queries.ListCodebaseInvestigationDrafts(ctx, artifact.JobID)
	if err != nil {
		return investigation.Draft{}, false, err
	}
	for _, row := range drafts {
		if row.AgentRunID != artifact.AgentRunID {
			continue
		}
		existing := investigation.Draft{JobID: row.JobID, AgentRunID: row.AgentRunID, ArtifactID: row.ArtifactID, CreatedAt: row.CreatedAt.UTC()}
		if existing.ArtifactID != artifact.ID {
			return investigation.Draft{}, false, fmt.Errorf("AgentRun %s already has a different immutable investigation draft", artifact.AgentRunID)
		}
		if err := insertArtifact(ctx, tx, artifact); err != nil {
			return investigation.Draft{}, false, err
		}
		return existing, false, nil
	}
	if !run.AdmissionOpen || run.CleanupState != core.CleanupPending {
		return investigation.Draft{}, false, fmt.Errorf("investigation draft cannot be recorded after admission closes or cleanup begins")
	}
	if !artifact.CreatedAt.Equal(run.FinishedAt.Time) {
		return investigation.Draft{}, false, fmt.Errorf("investigation draft timing conflicts with its completed AgentRun")
	}
	if err := insertArtifact(ctx, tx, artifact); err != nil {
		return investigation.Draft{}, false, err
	}
	if err := expectOneRows(queries.InsertCodebaseInvestigationDraft(ctx, dbsql.InsertCodebaseInvestigationDraftParams{
		JobID: artifact.JobID, AgentRunID: artifact.AgentRunID, ArtifactID: artifact.ID,
	})); err != nil {
		return investigation.Draft{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return investigation.Draft{}, false, err
	}
	stored := investigation.Draft{JobID: artifact.JobID, AgentRunID: artifact.AgentRunID, ArtifactID: artifact.ID, CreatedAt: artifact.CreatedAt.UTC()}
	return stored, true, nil
}

func validateInvestigationDraft(artifact core.Artifact) error {
	if strings.TrimSpace(artifact.JobID) == "" || strings.TrimSpace(artifact.AgentRunID) == "" ||
		!strings.HasPrefix(artifact.Name, "report-") || !strings.HasSuffix(artifact.Name, ".md") ||
		artifact.ID != core.ArtifactID(artifact.JobID, artifact.Name) || artifact.MediaType != "text/markdown" ||
		artifact.Producer != "dorf-codebase-investigation" || artifact.Digest == "" || artifact.ByteSize <= 0 || artifact.CreatedAt.IsZero() {
		return fmt.Errorf("codebase-investigation draft lacks its exact Markdown Artifact")
	}
	return nil
}
