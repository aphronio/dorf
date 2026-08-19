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

func (s Store) CodebaseInvestigationReport(ctx context.Context, jobID string) (*spine.CodebaseInvestigationReport, error) {
	row, err := dbsql.New(s.DB).GetCodebaseInvestigationReport(ctx, jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	report := investigationReport(row.JobID, row.ReportArtifactID, row.ObservedAt)
	return &report, nil
}

func (s Store) RecordCodebaseInvestigationReport(ctx context.Context, artifact spine.Artifact) (spine.CodebaseInvestigationReport, bool, error) {
	if err := validateInvestigationReport(artifact); err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	run, err := queries.GetCodebaseInvestigationRunForUpdate(ctx, dbsql.GetCodebaseInvestigationRunForUpdateParams{JobID: artifact.JobID, AgentRunID: artifact.AgentRunID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return spine.CodebaseInvestigationReport{}, false, ErrNotFound
		}
		return spine.CodebaseInvestigationReport{}, false, err
	}
	if run.WorkflowName != spine.WorkflowCodebaseInvestigation || run.WorkflowRevision != spine.CodebaseInvestigationRevision || run.Role != "investigate" ||
		run.State != spine.AgentRunCompleted || run.TurnID == "" || run.TurnOutcome != "completed" || run.InputRevision != run.Revision ||
		!run.StartedAt.Valid || !run.FinishedAt.Valid {
		return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("investigation Report conflicts with its completed exact-Revision AgentRun")
	}
	existingRow, err := queries.GetCodebaseInvestigationReport(ctx, artifact.JobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	if err == nil {
		existing := investigationReport(existingRow.JobID, existingRow.ReportArtifactID, existingRow.ObservedAt)
		if existing.ReportArtifactID != artifact.ID {
			return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("Job %s already has an immutable codebase-investigation Report", artifact.JobID)
		}
		if err := insertArtifact(ctx, tx, artifact); err != nil {
			return spine.CodebaseInvestigationReport{}, false, err
		}
		return existing, false, nil
	}
	if !run.AdmissionOpen || run.CleanupState != spine.CleanupPending {
		return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("investigation Report cannot be recorded after admission closes or cleanup begins")
	}
	if !artifact.CreatedAt.Equal(run.FinishedAt.Time) {
		return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("investigation Report timing conflicts with its completed AgentRun")
	}
	if err := insertArtifact(ctx, tx, artifact); err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	if err := expectOneRows(queries.InsertCodebaseInvestigationReport(ctx, dbsql.InsertCodebaseInvestigationReportParams{
		JobID: artifact.JobID, ReportArtifactID: artifact.ID,
	})); err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	if err := expectOneRows(queries.CloseAdmissionForCodebaseInvestigation(ctx, artifact.JobID)); err != nil {
		return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("close admission for investigation Report: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	stored := investigationReport(artifact.JobID, artifact.ID, artifact.CreatedAt)
	return stored, true, nil
}

func validateInvestigationReport(artifact spine.Artifact) error {
	if strings.TrimSpace(artifact.JobID) == "" || strings.TrimSpace(artifact.AgentRunID) == "" ||
		artifact.Name != spine.CodebaseInvestigationReportArtifactName ||
		artifact.ID != spine.ArtifactID(artifact.JobID, artifact.Name) || artifact.MediaType != "text/markdown" ||
		artifact.Producer != "dorf-codebase-investigation" || artifact.Digest == "" || artifact.ByteSize <= 0 || artifact.CreatedAt.IsZero() {
		return fmt.Errorf("codebase-investigation Report lacks its exact Markdown Artifact")
	}
	return nil
}

func investigationReport(jobID, artifactID string, observedAt time.Time) spine.CodebaseInvestigationReport {
	return spine.CodebaseInvestigationReport{JobID: jobID, ReportArtifactID: artifactID, ObservedAt: observedAt.UTC()}
}
