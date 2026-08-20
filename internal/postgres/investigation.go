package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/aphronio/dorf/internal/spine"
)

func (s Store) CodebaseInvestigationSource(ctx context.Context, jobID string) (spine.CodebaseInvestigationSource, error) {
	row, err := dbsql.New(s.DB).GetCodebaseInvestigationSource(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.CodebaseInvestigationSource{}, ErrNotFound
	}
	if err != nil {
		return spine.CodebaseInvestigationSource{}, err
	}
	return investigationSourceFromValues(row.JobID, row.Kind, row.Repository, row.Revision, row.BundleDigest, row.BundleByteSize), nil
}

func (s Store) CodebaseInvestigationDrafts(ctx context.Context, jobID string) ([]spine.CodebaseInvestigationDraft, error) {
	rows, err := dbsql.New(s.DB).ListCodebaseInvestigationDrafts(ctx, jobID)
	if err != nil {
		return nil, err
	}
	drafts := make([]spine.CodebaseInvestigationDraft, 0, len(rows))
	for _, row := range rows {
		drafts = append(drafts, spine.CodebaseInvestigationDraft{JobID: row.JobID, AgentRunID: row.AgentRunID, ArtifactID: row.ArtifactID, CreatedAt: row.CreatedAt.UTC()})
	}
	return drafts, nil
}

func (s Store) CodebaseInvestigationDecision(ctx context.Context, jobID string) (*spine.CodebaseInvestigationDecision, error) {
	row, err := dbsql.New(s.DB).GetCodebaseInvestigationDecision(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	decision := investigationDecision(row.JobID, row.ArtifactID, row.Disposition, row.DecidedBy, row.Reason, row.DecidedAt)
	return &decision, nil
}

func (s Store) RecordCodebaseInvestigationDraft(ctx context.Context, artifact spine.Artifact) (spine.CodebaseInvestigationDraft, bool, error) {
	if err := validateInvestigationDraft(artifact); err != nil {
		return spine.CodebaseInvestigationDraft{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.CodebaseInvestigationDraft{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	run, err := queries.GetCodebaseInvestigationRunForUpdate(ctx, dbsql.GetCodebaseInvestigationRunForUpdateParams{JobID: artifact.JobID, AgentRunID: artifact.AgentRunID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spine.CodebaseInvestigationDraft{}, false, ErrNotFound
		}
		return spine.CodebaseInvestigationDraft{}, false, err
	}
	if run.WorkflowName != spine.WorkflowCodebaseInvestigation || run.WorkflowRevision != spine.CodebaseInvestigationRevision || run.Role != "investigate" ||
		run.State != spine.AgentRunCompleted || run.TurnID == "" || run.TurnOutcome != "completed" || run.InputRevision != run.Revision ||
		!run.StartedAt.Valid || !run.FinishedAt.Valid {
		return spine.CodebaseInvestigationDraft{}, false, fmt.Errorf("investigation draft conflicts with its completed exact-Revision AgentRun")
	}
	drafts, err := queries.ListCodebaseInvestigationDrafts(ctx, artifact.JobID)
	if err != nil {
		return spine.CodebaseInvestigationDraft{}, false, err
	}
	for _, row := range drafts {
		if row.AgentRunID != artifact.AgentRunID {
			continue
		}
		existing := spine.CodebaseInvestigationDraft{JobID: row.JobID, AgentRunID: row.AgentRunID, ArtifactID: row.ArtifactID, CreatedAt: row.CreatedAt.UTC()}
		if existing.ArtifactID != artifact.ID {
			return spine.CodebaseInvestigationDraft{}, false, fmt.Errorf("AgentRun %s already has a different immutable investigation draft", artifact.AgentRunID)
		}
		if err := insertArtifact(ctx, tx, artifact); err != nil {
			return spine.CodebaseInvestigationDraft{}, false, err
		}
		return existing, false, nil
	}
	if !run.AdmissionOpen || run.CleanupState != spine.CleanupPending {
		return spine.CodebaseInvestigationDraft{}, false, fmt.Errorf("investigation draft cannot be recorded after admission closes or cleanup begins")
	}
	if !artifact.CreatedAt.Equal(run.FinishedAt.Time) {
		return spine.CodebaseInvestigationDraft{}, false, fmt.Errorf("investigation draft timing conflicts with its completed AgentRun")
	}
	if err := insertArtifact(ctx, tx, artifact); err != nil {
		return spine.CodebaseInvestigationDraft{}, false, err
	}
	if err := expectOneRows(queries.InsertCodebaseInvestigationDraft(ctx, dbsql.InsertCodebaseInvestigationDraftParams{
		JobID: artifact.JobID, AgentRunID: artifact.AgentRunID, ArtifactID: artifact.ID,
	})); err != nil {
		return spine.CodebaseInvestigationDraft{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.CodebaseInvestigationDraft{}, false, err
	}
	stored := spine.CodebaseInvestigationDraft{JobID: artifact.JobID, AgentRunID: artifact.AgentRunID, ArtifactID: artifact.ID, CreatedAt: artifact.CreatedAt.UTC()}
	return stored, true, nil
}

type NewCodebaseInvestigationDecision struct {
	JobID       string
	ArtifactID  string
	Disposition spine.InvestigationDisposition
	DecidedBy   string
	Reason      string
}

func (s Store) RecordCodebaseInvestigationDecision(ctx context.Context, input NewCodebaseInvestigationDecision) (spine.CodebaseInvestigationDecision, bool, error) {
	input.JobID = strings.TrimSpace(input.JobID)
	input.ArtifactID = strings.TrimSpace(input.ArtifactID)
	input.DecidedBy = strings.TrimSpace(input.DecidedBy)
	input.Reason = strings.TrimSpace(input.Reason)
	if input.JobID == "" || input.ArtifactID == "" || input.DecidedBy == "" || len(input.DecidedBy) > 256 || len(input.Reason) > 1<<20 ||
		(input.Disposition != spine.InvestigationAccepted && input.Disposition != spine.InvestigationRejected) {
		return spine.CodebaseInvestigationDecision{}, false, fmt.Errorf("investigation decision requires an exact draft, accept or reject disposition, and human identity")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.CodebaseInvestigationDecision{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	job, err := queries.GetCodebaseInvestigationJobForUpdate(ctx, input.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return spine.CodebaseInvestigationDecision{}, false, ErrNotFound
	}
	if err != nil {
		return spine.CodebaseInvestigationDecision{}, false, err
	}
	existingRow, err := queries.GetCodebaseInvestigationDecision(ctx, input.JobID)
	if err == nil {
		existing := investigationDecision(existingRow.JobID, existingRow.ArtifactID, existingRow.Disposition, existingRow.DecidedBy, existingRow.Reason, existingRow.DecidedAt)
		if existing.ArtifactID != input.ArtifactID || existing.Disposition != input.Disposition || existing.DecidedBy != input.DecidedBy || existing.Reason != input.Reason {
			return spine.CodebaseInvestigationDecision{}, false, fmt.Errorf("Job %s already has a different immutable investigation decision", input.JobID)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return spine.CodebaseInvestigationDecision{}, false, err
	}
	if job.WorkflowName != spine.WorkflowCodebaseInvestigation || job.WorkflowRevision != spine.CodebaseInvestigationRevision || !job.AdmissionOpen || job.CleanupState != spine.CleanupPending {
		return spine.CodebaseInvestigationDecision{}, false, fmt.Errorf("Job %s does not accept an investigation decision", input.JobID)
	}
	latest, err := queries.GetLatestInvestigationRunAndDraft(ctx, input.JobID)
	if err != nil {
		return spine.CodebaseInvestigationDecision{}, false, err
	}
	if latest.State != spine.AgentRunCompleted || latest.ArtifactID == "" || latest.ArtifactID != input.ArtifactID {
		return spine.CodebaseInvestigationDecision{}, false, fmt.Errorf("decision must reference the latest completed investigation draft")
	}
	if err := expectOneRows(queries.InsertCodebaseInvestigationDecision(ctx, dbsql.InsertCodebaseInvestigationDecisionParams{
		JobID: input.JobID, ArtifactID: input.ArtifactID, Disposition: string(input.Disposition), DecidedBy: input.DecidedBy, Reason: input.Reason,
	})); err != nil {
		return spine.CodebaseInvestigationDecision{}, false, err
	}
	if err := expectOneRows(queries.CloseAdmissionForCodebaseInvestigation(ctx, input.JobID)); err != nil {
		return spine.CodebaseInvestigationDecision{}, false, fmt.Errorf("close admission for investigation decision: %w", err)
	}
	storedRow, err := queries.GetCodebaseInvestigationDecision(ctx, input.JobID)
	if err != nil {
		return spine.CodebaseInvestigationDecision{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return spine.CodebaseInvestigationDecision{}, false, err
	}
	return investigationDecision(storedRow.JobID, storedRow.ArtifactID, storedRow.Disposition, storedRow.DecidedBy, storedRow.Reason, storedRow.DecidedAt), true, nil
}

func validateInvestigationDraft(artifact spine.Artifact) error {
	if strings.TrimSpace(artifact.JobID) == "" || strings.TrimSpace(artifact.AgentRunID) == "" ||
		!strings.HasPrefix(artifact.Name, "report-") || !strings.HasSuffix(artifact.Name, ".md") ||
		artifact.ID != spine.ArtifactID(artifact.JobID, artifact.Name) || artifact.MediaType != "text/markdown" ||
		artifact.Producer != "dorf-codebase-investigation" || artifact.Digest == "" || artifact.ByteSize <= 0 || artifact.CreatedAt.IsZero() {
		return fmt.Errorf("codebase-investigation draft lacks its exact Markdown Artifact")
	}
	return nil
}

func investigationDecision(jobID, artifactID, disposition, decidedBy, reason string, decidedAt time.Time) spine.CodebaseInvestigationDecision {
	return spine.CodebaseInvestigationDecision{JobID: jobID, ArtifactID: artifactID, Disposition: spine.InvestigationDisposition(disposition), DecidedBy: decidedBy, Reason: reason, DecidedAt: decidedAt.UTC()}
}
