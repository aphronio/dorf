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

func normalizeCoreAdmission(input core.JobAdmission) (core.JobAdmission, error) {
	input.AdmissionKey = strings.TrimSpace(input.AdmissionKey)
	input.Workflow = core.WorkflowName(strings.TrimSpace(string(input.Workflow)))
	input.WorkflowRevision = strings.TrimSpace(input.WorkflowRevision)
	input.SandboxProfile = strings.TrimSpace(input.SandboxProfile)
	input.ProviderConnection = strings.TrimSpace(input.ProviderConnection)
	input.Model = strings.TrimSpace(input.Model)
	input.ReasoningEffort = strings.TrimSpace(input.ReasoningEffort)
	if !core.ValidClientReference(input.ClientReference) {
		return core.JobAdmission{}, fmt.Errorf("invalid client reference")
	}
	if (input.Workflow == "") != (input.WorkflowRevision == "") {
		return core.JobAdmission{}, fmt.Errorf("workflow name and revision must either both be absent or both be present")
	}
	if input.AdmissionKey == "" || input.SandboxProfile == "" || input.ProviderConnection == "" || input.Model == "" {
		return core.JobAdmission{}, fmt.Errorf("admission requires key, Sandbox profile, AI connection, model")
	}
	if input.ReasoningEffort != "low" && input.ReasoningEffort != "medium" && input.ReasoningEffort != "high" && input.ReasoningEffort != "xhigh" {
		return core.JobAdmission{}, fmt.Errorf("reasoning effort must be low, medium, high, or xhigh")
	}
	return input, nil
}

func admitJob(ctx context.Context, store Store, coreInput core.JobAdmission, queueName, taskName, taskKey string, recordTypedFacts func(context.Context, *dbsql.Queries, string) error) (core.Job, bool, error) {
	id := core.JobID(coreInput.AdmissionKey)
	tx, err := store.DB.BeginTx(ctx, nil)
	if err != nil {
		return core.Job{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(store.DB).WithTx(tx)
	storedRow, err := queries.GetAdmittedJobForUpdate(ctx, coreInput.AdmissionKey)
	var rows int64
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := queries.LockVerifiedSandboxProfileForAdmission(ctx, dbsql.LockVerifiedSandboxProfileForAdmissionParams{
			Name: coreInput.SandboxProfile, ContractVersion: core.BaseProfileContract,
		}); errors.Is(err, sql.ErrNoRows) {
			return core.Job{}, false, fmt.Errorf("Sandbox profile %q has not completed Dorf %s verification and cleanup", coreInput.SandboxProfile, core.BaseProfileContract)
		} else if err != nil {
			return core.Job{}, false, err
		}
		rows, err = queries.InsertAdmittedJob(ctx, dbsql.InsertAdmittedJobParams{
			CreatedByClientID: coreInput.CreatedByClientID, ClientReference: coreInput.ClientReference,
			ID: id, AdmissionKey: coreInput.AdmissionKey, WorkflowName: coreInput.Workflow, WorkflowRevision: coreInput.WorkflowRevision,
			AgentsMd: coreInput.AgentsMD, SandboxProfile: coreInput.SandboxProfile, ProviderConnection: coreInput.ProviderConnection,
			Model: coreInput.Model, ReasoningEffort: coreInput.ReasoningEffort,
		})
		if err != nil {
			return core.Job{}, false, err
		}
		storedRow, err = queries.GetAdmittedJobForUpdate(ctx, coreInput.AdmissionKey)
	}
	if err != nil {
		return core.Job{}, false, err
	}
	storedCore := core.JobAdmission{
		ClientReference: storedRow.ClientReference,
		AdmissionKey:    storedRow.AdmissionKey, Workflow: core.WorkflowName(storedRow.WorkflowName), WorkflowRevision: storedRow.WorkflowRevision,
		AgentsMD: storedRow.AgentsMd, SandboxProfile: storedRow.SandboxProfile, ProviderConnection: storedRow.ProviderConnection,
		Model: storedRow.Model, ReasoningEffort: storedRow.ReasoningEffort,
	}
	// A replay may come from another Client; only the first admission records its creator.
	comparison := coreInput
	comparison.CreatedByClientID = ""
	if storedRow.ID != id || storedCore != comparison {
		return core.Job{}, false, fmt.Errorf("%w: %q", ErrAdmissionConflict, coreInput.AdmissionKey)
	}
	sandboxID := core.MainSandboxName(id)
	if err := reserveAdmittedSandbox(ctx, queries, id, sandboxID); err != nil {
		return core.Job{}, false, err
	}
	if recordTypedFacts != nil {
		if err := recordTypedFacts(ctx, queries, id); err != nil {
			return core.Job{}, false, err
		}
	}
	if err := scheduleJobTaskTx(ctx, tx, queueName, id, taskName, taskKey, true); err != nil {
		return core.Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return core.Job{}, false, err
	}
	job, err := store.Job(ctx, id)
	return job, rows == 1, err
}

func reserveAdmittedSandbox(ctx context.Context, queries *dbsql.Queries, id, sandboxID string) error {
	ownerNonce, err := reviewNonce()
	if err != nil {
		return err
	}
	if err := expectOneRows(queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: sandboxID, JobID: id, Name: core.DefaultSandbox, OwnershipNonce: ownerNonce})); err != nil {
		reserved, getErr := queries.GetSandbox(ctx, sandboxID)
		if getErr != nil {
			return err
		}
		if reserved.ID != sandboxID || reserved.JobID != id || reserved.Name != core.DefaultSandbox || !sha256Digest.MatchString(reserved.OwnershipNonce) {
			return fmt.Errorf("Job %s default Sandbox conflicts with its exact owned identity", id)
		}
	}
	return nil
}
