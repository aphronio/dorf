package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func normalizeCoreAdmission(input core.SessionAdmission) (core.SessionAdmission, error) {
	input.AdmissionKey = strings.TrimSpace(input.AdmissionKey)
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	input.SandboxProfile = strings.TrimSpace(input.SandboxProfile)
	input.ProviderConnection = strings.TrimSpace(input.ProviderConnection)
	input.Model = strings.TrimSpace(input.Model)
	input.ReasoningEffort = strings.TrimSpace(input.ReasoningEffort)
	if !core.ValidClientReference(input.ClientReference) {
		return core.SessionAdmission{}, fmt.Errorf("invalid client reference")
	}
	if (input.Workflow == "") != (input.WorkflowRevision == "") {
		return core.SessionAdmission{}, fmt.Errorf("workflow name and revision must either both be absent or both be present")
	}
	if input.AdmissionKey == "" || input.SandboxProfile == "" || input.ProviderConnection == "" || input.Model == "" {
		return core.SessionAdmission{}, fmt.Errorf("admission requires key, Sandbox profile, AI connection, model")
	}
	if input.ReasoningEffort != "low" && input.ReasoningEffort != "medium" && input.ReasoningEffort != "high" && input.ReasoningEffort != "xhigh" {
		return core.SessionAdmission{}, fmt.Errorf("reasoning effort must be low, medium, high, or xhigh")
	}
	return input, nil
}

func admitSession(ctx context.Context, store Store, coreInput core.SessionAdmission, queueName, taskName, taskKey string) (core.Session, bool, error) {
	id := core.SessionID(coreInput.AdmissionKey)
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.Session{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(store.DB).WithTx(tx)
	storedRow, err := queries.GetAdmittedSessionForUpdate(ctx, coreInput.AdmissionKey)
	var rows int64
	if errors.Is(err, sql.ErrNoRows) {
		var revision string
		revision, err = lockAdmissionProfileRevision(ctx, queries, coreInput.SandboxProfile)
		if err != nil {
			return core.Session{}, false, err
		}
		rows, err = queries.InsertAdmittedSession(ctx, dbsql.InsertAdmittedSessionParams{
			CreatedByClientID: coreInput.CreatedByClientID, ClientReference: coreInput.ClientReference,
			ID: id, AdmissionKey: coreInput.AdmissionKey, WorkflowName: coreInput.Workflow, WorkflowRevision: coreInput.WorkflowRevision,
			AgentsMd: coreInput.AgentsMD, SandboxProfile: coreInput.SandboxProfile, SandboxProfileRevision: revision, ProviderConnection: coreInput.ProviderConnection,
			KeepRunning: coreInput.KeepRunning, Model: coreInput.Model, ReasoningEffort: coreInput.ReasoningEffort,
		})
		if err != nil {
			return core.Session{}, false, err
		}
		storedRow, err = queries.GetAdmittedSessionForUpdate(ctx, coreInput.AdmissionKey)
	}
	if err != nil {
		return core.Session{}, false, err
	}
	storedCore := core.SessionAdmission{
		ClientReference: storedRow.ClientReference,
		AdmissionKey:    storedRow.AdmissionKey, Workflow: core.WorkflowName(storedRow.WorkflowName), WorkflowRevision: storedRow.WorkflowRevision,
		AgentsMD: storedRow.AgentsMd, SandboxProfile: storedRow.SandboxProfile, ProviderConnection: storedRow.ProviderConnection,
		KeepRunning: storedRow.KeepRunning, Model: storedRow.Model, ReasoningEffort: storedRow.ReasoningEffort,
	}
	// A replay may come from another Client; only the first admission records its creator.
	comparison := coreInput
	comparison.CreatedByClientID = ""
	if storedRow.ID != id || storedCore != comparison {
		return core.Session{}, false, fmt.Errorf("%w: %q", ErrAdmissionConflict, coreInput.AdmissionKey)
	}
	sandboxID := core.MainSandboxName(id)
	if err := reserveAdmittedSandbox(ctx, queries, id, sandboxID); err != nil {
		return core.Session{}, false, err
	}
	if err := scheduleSessionTaskTx(ctx, tx, queueName, id, taskName, taskKey, true); err != nil {
		return core.Session{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return core.Session{}, false, err
	}
	session, err := store.Session(ctx, id)
	return session, rows == 1, err
}

func reserveAdmittedSandbox(ctx context.Context, queries *dbsql.Queries, id, sandboxID string) error {
	ownerNonce, err := ownershipNonce()
	if err != nil {
		return err
	}
	if err := expectOneRows(queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: sandboxID, SessionID: id, Name: core.DefaultSandbox, OwnershipNonce: ownerNonce})); err != nil {
		reserved, getErr := queries.GetSandbox(ctx, sandboxID)
		if getErr != nil {
			return err
		}
		if reserved.ID != sandboxID || reserved.SessionID != id || reserved.Name != core.DefaultSandbox || !sha256Digest.MatchString(reserved.OwnershipNonce) {
			return fmt.Errorf("Session %s default Sandbox conflicts with its exact owned identity", id)
		}
	}
	return nil
}

// Lock the name before reading its receipt. A single joined SELECT FOR SHARE
// can retain the old receipt in its snapshot while waiting for promotion, then
// reject the newly active revision during PostgreSQL's row recheck.
func lockAdmissionProfileRevision(ctx context.Context, queries *dbsql.Queries, name string) (string, error) {
	_, err := queries.LockSandboxProfileNameForAdmission(ctx, name)
	var revision string
	if err == nil {
		revision, err = queries.LockVerifiedSandboxProfileForAdmission(ctx, dbsql.LockVerifiedSandboxProfileForAdmissionParams{Name: name, ContractVersion: core.BaseProfileContract})
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", name, core.BaseProfileContract)
	}
	return revision, err
}
