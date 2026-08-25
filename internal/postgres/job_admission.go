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
	if (input.Workflow == "") != (input.WorkflowRevision == "") {
		return core.JobAdmission{}, fmt.Errorf("workflow name and revision must either both be absent or both be present")
	}
	if input.AdmissionKey == "" || strings.TrimSpace(input.Goal) == "" || input.SandboxProfile == "" || input.ProviderConnection == "" || input.Model == "" {
		return core.JobAdmission{}, fmt.Errorf("admission requires key, complete goal, Sandbox profile, AI connection, and model")
	}
	if input.ReasoningEffort != "low" && input.ReasoningEffort != "medium" && input.ReasoningEffort != "high" && input.ReasoningEffort != "xhigh" {
		return core.JobAdmission{}, fmt.Errorf("reasoning effort must be low, medium, high, or xhigh")
	}
	return input, nil
}

type admittedJobIDs struct{ jobID, messageID, sandboxID string }

func admitJob(ctx context.Context, store Store, coreInput core.JobAdmission, recordTypedFacts func(context.Context, *dbsql.Queries, admittedJobIDs) error) (core.Job, bool, error) {
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
			ID: id, AdmissionKey: coreInput.AdmissionKey, WorkflowName: coreInput.Workflow, WorkflowRevision: coreInput.WorkflowRevision,
			Goal: coreInput.Goal, SandboxProfile: coreInput.SandboxProfile, ProviderConnection: coreInput.ProviderConnection,
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
		AdmissionKey: storedRow.AdmissionKey, Workflow: core.WorkflowName(storedRow.WorkflowName), WorkflowRevision: storedRow.WorkflowRevision,
		Goal: storedRow.Goal, SandboxProfile: storedRow.SandboxProfile, ProviderConnection: storedRow.ProviderConnection,
		Model: storedRow.Model, ReasoningEffort: storedRow.ReasoningEffort,
	}
	if storedRow.ID != id || storedCore != coreInput {
		return core.Job{}, false, fmt.Errorf("admission key %q is already bound to different complete Job input", coreInput.AdmissionKey)
	}
	messageID := core.MessageID(id, core.MessageFromHuman, initialFromID)
	if err := queries.InsertInitialMessage(ctx, dbsql.InsertInitialMessageParams{ID: messageID, JobID: id, FromID: initialFromID, Input: coreInput.Goal}); err != nil {
		return core.Job{}, false, err
	}
	initial, err := queries.GetMessageBySender(ctx, dbsql.GetMessageBySenderParams{JobID: id, FromKind: core.MessageFromHuman, FromID: initialFromID})
	if err != nil {
		return core.Job{}, false, err
	}
	if initial.ID != messageID || initial.JobID != id || initial.FromKind != core.MessageFromHuman || initial.FromID != initialFromID ||
		initial.Sequence != 1 || initial.Input != coreInput.Goal || initial.DeliveryIntent != core.MessageFollow || initial.SteerTargetTurnID != "" {
		return core.Job{}, false, fmt.Errorf("Job %s initial message conflicts with complete admission input", id)
	}
	sandboxID := core.MainSandboxName(id)
	ownerNonce, err := reviewNonce()
	if err != nil {
		return core.Job{}, false, err
	}
	if err := expectOneRows(queries.ReserveSandbox(ctx, dbsql.ReserveSandboxParams{ID: sandboxID, JobID: id, Name: core.DefaultSandbox, OwnershipNonce: ownerNonce})); err != nil {
		reserved, getErr := queries.GetSandbox(ctx, sandboxID)
		if getErr != nil {
			return core.Job{}, false, err
		}
		if reserved.ID != sandboxID || reserved.JobID != id || reserved.Name != core.DefaultSandbox || !sha256Digest.MatchString(reserved.OwnershipNonce) {
			return core.Job{}, false, fmt.Errorf("Job %s default Sandbox conflicts with its exact owned identity", id)
		}
	}
	if err := recordTypedFacts(ctx, queries, admittedJobIDs{jobID: id, messageID: initial.ID, sandboxID: sandboxID}); err != nil {
		return core.Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return core.Job{}, false, err
	}
	job, err := store.Job(ctx, id)
	return job, rows == 1, err
}
