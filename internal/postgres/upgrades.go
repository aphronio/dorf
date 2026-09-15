package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/upgrade"
)

// RequestSandboxUpgrade commits exact package intent and its delivery hold with
// the execution wake. The retained direct task executes it; no second task owns
// the same Job lifecycle.
func (s Store) RequestSandboxUpgrade(ctx context.Context, queue string, request upgrade.Request) (upgrade.Receipt, error) {
	if err := request.Validate(); err != nil {
		return upgrade.Receipt{}, err
	}
	var result upgrade.Receipt
	err := s.withDeliveryHold(ctx, request.JobID, request.SandboxID, request.ID, func(tx *sql.Tx, q *dbsql.Queries, job dbsql.GetJobAdmissionForUpdateRow) error {
		row, err := q.GetSandboxUpgrade(ctx, request.ID)
		if err == nil {
			result = upgradeReceipt(row)
			if result.Request != request {
				return fmt.Errorf("upgrade identity already has different input")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !job.AdmissionOpen || job.CleanupState != core.CleanupPending || job.WorkflowName != "" || job.WorkflowRevision != "" {
			return fmt.Errorf("upgrade requires an open direct Job")
		}
		if err := requireSandboxDeliveryUnheld(ctx, q, request.SandboxID); err != nil {
			return err
		}
		owned, err := q.GetSandbox(ctx, request.SandboxID)
		if err != nil {
			return err
		}
		if owned.ProviderID == "" {
			return fmt.Errorf("upgrade requires an attested existing resource")
		}
		if err := q.InsertSandboxDeliveryHold(ctx, dbsql.InsertSandboxDeliveryHoldParams{ID: request.ID, SandboxID: request.SandboxID}); err != nil {
			return err
		}
		if err := q.InsertSandboxUpgrade(ctx, dbsql.InsertSandboxUpgradeParams{ID: request.ID, SandboxID: request.SandboxID, SourceResourceID: owned.ActiveResourceID, PackagePath: request.PackagePath, Version: request.Version}); err != nil {
			return err
		}
		if _, err := signalJobExecutionWakeTx(ctx, tx, queue, request.JobID, "upgrade:"+request.ID); err != nil {
			return err
		}
		row, err = q.GetSandboxUpgrade(ctx, request.ID)
		result = upgradeReceipt(row)
		return err
	})
	return result, err
}

func (s Store) JobUpgrades(ctx context.Context, jobID string) ([]upgrade.Receipt, error) {
	rows, err := dbsql.New(s.DB).ListJobUpgrades(ctx, jobID)
	if err != nil {
		return nil, err
	}
	result := make([]upgrade.Receipt, 0, len(rows))
	for _, row := range rows {
		result = append(result, upgradeReceipt(dbsql.GetSandboxUpgradeRow(row)))
	}
	return result, nil
}

func upgradeReceipt(row dbsql.GetSandboxUpgradeRow) upgrade.Receipt {
	return upgrade.Receipt{
		Request:          upgrade.Request{ID: row.ID, JobID: row.JobID, SandboxID: row.SandboxID, PackagePath: row.PackagePath, Version: row.Version},
		SourceResourceID: row.SourceResourceID, SourceProviderID: row.SourceProviderID, DestinationProviderID: row.DestinationProviderID, DestinationResourceID: row.DestinationResourceID.String,
		RequestedAt: row.RequestedAt, PreviousVersion: row.PreviousVersion.String, QuiescedAt: timeValue(row.QuiescedAt),
		Checkpoint:  provider.Checkpoint{Key: row.CheckpointKey.String, Reference: row.CheckpointReference.String, SourceID: row.CheckpointSourceID.String},
		ActivatedAt: timeValue(row.ActivatedAt), RollbackAt: timeValue(row.RollbackAt), FailureCode: row.FailureCode.String,
		RestoredAt: timeValue(row.RestoredAt), VerifiedAt: timeValue(row.VerifiedAt), CheckpointDeletedAt: timeValue(row.CheckpointDeletedAt), FinishedAt: timeValue(row.FinishedAt),
	}
}

func (s Store) UpgradeQuiescent(ctx context.Context, sandboxID string) (bool, error) {
	return dbsql.New(s.DB).UpgradeQuiescent(ctx, sandboxID)
}

// Upgrade receipt writers run only under the executor's Job fence and current
// claim. They never take another fence on a different database connection.
func (s Store) RecordUpgradePreparation(ctx context.Context, id, version string) error {
	return expectOneRows(dbsql.New(s.DB).RecordUpgradePreparation(ctx, dbsql.RecordUpgradePreparationParams{ID: id, PreviousVersion: version}))
}
func (s Store) RecordUpgradeQuiesced(ctx context.Context, id string) error {
	return expectOneRows(dbsql.New(s.DB).RecordUpgradeQuiesced(ctx, id))
}
func (s Store) RecordUpgradeCheckpoint(ctx context.Context, id string, checkpoint provider.Checkpoint) error {
	if checkpoint.Key != id || checkpoint.Reference == "" || checkpoint.SourceID == "" {
		return fmt.Errorf("checkpoint omitted exact upgrade identity")
	}
	return expectOneRows(dbsql.New(s.DB).RecordUpgradeCheckpoint(ctx, dbsql.RecordUpgradeCheckpointParams{ID: id, CheckpointKey: checkpoint.Key, CheckpointReference: checkpoint.Reference, CheckpointSourceID: checkpoint.SourceID}))
}
func (s Store) RecordUpgradeActivated(ctx context.Context, id string) error {
	return expectOneRows(dbsql.New(s.DB).RecordUpgradeActivated(ctx, id))
}
func (s Store) RequestUpgradeRollback(ctx context.Context, id, code string) error {
	return expectOneRows(dbsql.New(s.DB).RequestUpgradeRollback(ctx, dbsql.RequestUpgradeRollbackParams{ID: id, FailureCode: code}))
}
func (s Store) ReserveUpgradeDestination(ctx context.Context, receipt upgrade.Receipt, replace bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := dbsql.New(tx)
	row, err := q.GetSandboxUpgrade(ctx, receipt.ID)
	if err != nil {
		return err
	}
	if row.DestinationResourceID.Valid {
		return nil
	}
	resourceID := row.SourceResourceID
	if replace {
		resourceID = receipt.ID + ":replacement"
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		if err := q.ReserveSandboxResource(ctx, dbsql.ReserveSandboxResourceParams{ID: resourceID, SandboxID: receipt.SandboxID, OwnershipNonce: hex.EncodeToString(nonce[:])}); err != nil {
			return err
		}
	}
	if err := expectOneRows(q.BindUpgradeDestination(ctx, dbsql.BindUpgradeDestinationParams{ID: receipt.ID, ResourceID: resourceID})); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Store) RecordUpgradeRestored(ctx context.Context, receipt upgrade.Receipt, providerID string) error {
	if providerID == "" {
		return fmt.Errorf("recovery omitted provider resource identity")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := dbsql.New(tx)
	if err := expectOneRows(q.BindRecoveredResource(ctx, dbsql.BindRecoveredResourceParams{ResourceID: receipt.DestinationResourceID, ProviderID: providerID})); err != nil {
		return err
	}
	if err := expectOneRows(q.RecordUpgradeRestored(ctx, receipt.ID)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s Store) RecordUpgradeVerified(ctx context.Context, id string) error {
	return expectOneRows(dbsql.New(s.DB).RecordUpgradeVerified(ctx, id))
}
func (s Store) RecordUpgradeCheckpointDeleted(ctx context.Context, id string) error {
	return expectOneRows(dbsql.New(s.DB).RecordUpgradeCheckpointDeleted(ctx, id))
}

// FinishSandboxUpgrade atomically adopts verified custody, releases its exact
// hold, and wakes the existing task. Caller holds the Job fence across native
// verification and this commit. A failed wake rolls the whole transaction back.
func (s Store) FinishSandboxUpgrade(ctx context.Context, queue string, expected upgrade.Receipt) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := dbsql.New(tx)
	job, err := q.GetJobAdmissionForUpdate(ctx, expected.JobID)
	if err != nil {
		return err
	}
	if !job.AdmissionOpen || job.CleanupState != core.CleanupPending {
		return fmt.Errorf("upgrade release requires open admission")
	}
	row, err := q.GetSandboxUpgrade(ctx, expected.ID)
	if err != nil {
		return err
	}
	receipt := upgradeReceipt(row)
	if receipt.Request != expected.Request || receipt.SourceResourceID != expected.SourceResourceID || receipt.DestinationResourceID != expected.DestinationResourceID {
		return fmt.Errorf("upgrade custody changed")
	}
	if !receipt.FinishedAt.IsZero() {
		return nil
	}
	if err := authorizeUpgradeRelease(ctx, q, receipt); err != nil {
		return err
	}
	if err := switchAndReleaseUpgrade(ctx, tx, q, queue, receipt); err != nil {
		return err
	}
	return tx.Commit()
}

func authorizeUpgradeRelease(ctx context.Context, q *dbsql.Queries, receipt upgrade.Receipt) error {
	hold, err := q.GetSandboxDeliveryHold(ctx, receipt.ID)
	if err != nil {
		return err
	}
	if hold.SandboxID != receipt.SandboxID || hold.ReleasedAt.Valid || receipt.VerifiedAt.IsZero() {
		return fmt.Errorf("upgrade requires its held verified recovery receipt")
	}
	quiet, err := q.UpgradeQuiescent(ctx, receipt.SandboxID)
	if err != nil {
		return err
	}
	if !quiet {
		return fmt.Errorf("native delivery is not quiescent")
	}
	resources, err := q.ListSandboxResources(ctx, receipt.JobID)
	if err != nil {
		return err
	}
	return upgradeResourcesReleasable(receipt, resources)
}

func switchAndReleaseUpgrade(ctx context.Context, tx *sql.Tx, q *dbsql.Queries, queue string, receipt upgrade.Receipt) error {
	destination := receipt.DestinationResourceID
	if destination == "" {
		destination = receipt.SourceResourceID
	}
	if err := expectOneRows(q.SwitchSandboxResource(ctx, dbsql.SwitchSandboxResourceParams{SandboxID: receipt.SandboxID, SourceResourceID: receipt.SourceResourceID, DestinationResourceID: destination})); err != nil {
		return err
	}
	if err := expectOneRows(q.RecordUpgradeFinished(ctx, receipt.ID)); err != nil {
		return err
	}
	if err := expectOneRows(q.ReleaseSandboxDeliveryHold(ctx, dbsql.ReleaseSandboxDeliveryHoldParams{ID: receipt.ID, SandboxID: receipt.SandboxID})); err != nil {
		return err
	}
	if _, err := signalJobExecutionWakeTx(ctx, tx, queue, receipt.JobID, "upgrade-finished:"+receipt.ID); err != nil {
		return err
	}
	return nil
}

func upgradeResourcesReleasable(receipt upgrade.Receipt, resources []dbsql.ListSandboxResourcesRow) error {
	target := receipt.SourceResourceID
	if receipt.DestinationResourceID != "" {
		target = receipt.DestinationResourceID
	}
	for _, resource := range resources {
		if resource.ID == target && (resource.ProviderID == "" || resource.DeletedAt.Valid) {
			return fmt.Errorf("verified destination is absent")
		}
		if resource.ID == receipt.SourceResourceID && target != receipt.SourceResourceID && !resource.DeletedAt.Valid {
			return fmt.Errorf("old resource cleanup is not verified")
		}
	}
	if target == receipt.SourceResourceID && receipt.CheckpointDeletedAt.IsZero() {
		return fmt.Errorf("unused checkpoint cleanup is not verified")
	}
	return nil
}
