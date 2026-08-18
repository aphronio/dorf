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
	report := investigationReport(row.JobID, row.AgentRunID, row.ReportEvidenceID, row.ObservedAt)
	return &report, nil
}

func (s Store) RecordCodebaseInvestigationReport(ctx context.Context, receipt spine.CodebaseInvestigationReport, observed spine.Evidence) (spine.CodebaseInvestigationReport, bool, error) {
	if err := validateInvestigationReport(receipt, observed); err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(s.DB).WithTx(tx)
	run, err := queries.GetCodebaseInvestigationRunForUpdate(ctx, dbsql.GetCodebaseInvestigationRunForUpdateParams{JobID: receipt.JobID, AgentRunID: receipt.AgentRunID})
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
	existingRow, err := queries.GetCodebaseInvestigationReport(ctx, receipt.JobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	if err == nil {
		existing := investigationReport(existingRow.JobID, existingRow.AgentRunID, existingRow.ReportEvidenceID, existingRow.ObservedAt)
		if existing.AgentRunID != receipt.AgentRunID || existing.ReportEvidenceID != receipt.ReportEvidenceID {
			return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("Job %s already has an immutable codebase-investigation Report", receipt.JobID)
		}
		if err := insertEvidence(ctx, tx, receipt.JobID, observed); err != nil {
			return spine.CodebaseInvestigationReport{}, false, err
		}
		return existing, false, nil
	}
	if !run.AdmissionOpen || run.CleanupState != spine.CleanupPending {
		return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("investigation Report cannot be recorded after admission closes or cleanup begins")
	}
	if !receipt.ObservedAt.Equal(run.FinishedAt.Time) || !observed.StartedAt.Equal(run.StartedAt.Time) || !observed.FinishedAt.Equal(run.FinishedAt.Time) {
		return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("investigation Report timing conflicts with its completed AgentRun")
	}
	if err := insertEvidence(ctx, tx, receipt.JobID, observed); err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	inserted, err := queries.InsertCodebaseInvestigationReport(ctx, dbsql.InsertCodebaseInvestigationReportParams{
		JobID: receipt.JobID, AgentRunID: receipt.AgentRunID,
		ReportEvidenceID: receipt.ReportEvidenceID, ObservedAt: receipt.ObservedAt,
	})
	if err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	if err := expectOneRows(queries.CloseAdmissionForCodebaseInvestigation(ctx, receipt.JobID)); err != nil {
		return spine.CodebaseInvestigationReport{}, false, fmt.Errorf("close admission for investigation Report: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return spine.CodebaseInvestigationReport{}, false, err
	}
	stored := investigationReport(inserted.JobID, inserted.AgentRunID, inserted.ReportEvidenceID, inserted.ObservedAt)
	return stored, true, nil
}

func validateInvestigationReport(receipt spine.CodebaseInvestigationReport, observed spine.Evidence) error {
	if strings.TrimSpace(receipt.JobID) == "" || strings.TrimSpace(receipt.AgentRunID) == "" || receipt.ObservedAt.IsZero() {
		return fmt.Errorf("codebase-investigation Report receipt is incomplete")
	}
	if receipt.ReportEvidenceID != spine.EvidenceID(receipt.AgentRunID, "investigation-report") || observed.ID != receipt.ReportEvidenceID ||
		observed.AgentRunID != receipt.AgentRunID || observed.Kind != "investigation-report" || observed.ActionID != "" || observed.CheckID != "" ||
		observed.Revision == "" || observed.MediaType != "text/markdown" || observed.Digest == "" || observed.ByteSize <= 0 {
		return fmt.Errorf("codebase-investigation Report lacks its exact report Evidence")
	}
	return nil
}

func investigationReport(jobID, runID, evidenceID string, observedAt time.Time) spine.CodebaseInvestigationReport {
	return spine.CodebaseInvestigationReport{JobID: jobID, AgentRunID: runID, ReportEvidenceID: evidenceID, ObservedAt: observedAt.UTC()}
}
