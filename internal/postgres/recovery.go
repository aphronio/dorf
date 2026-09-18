package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// RequestCheckpointRecovery atomically retains exact checkpoint intent, holds
// new native delivery, and wakes the Session's existing task. Accepted Messages
// remain durable behind the hold.
func (s Store) RequestCheckpointRecovery(ctx context.Context, queue string, request persistence.RecoveryRequest) (persistence.RecoveryReceipt, error) {
	if err := request.Validate(); err != nil {
		return persistence.RecoveryReceipt{}, err
	}
	var result persistence.RecoveryReceipt
	err := s.withDeliveryHold(ctx, request.SessionID, request.SandboxID, request.ID, func(tx *sql.Tx, q *dbsql.Queries, session dbsql.GetSessionAdmissionForUpdateRow) error {
		var err error
		if result, err = replayCheckpointRecovery(ctx, q, request); err != nil || result.ID != "" {
			return err
		}
		if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
			return fmt.Errorf("checkpoint recovery requires an open direct Session")
		}
		owned, err := authorizeCheckpointRecoveryRequest(ctx, q, request)
		if err != nil {
			return err
		}
		if err := requireSandboxDeliveryUnheld(ctx, q, request.SandboxID); err != nil {
			return err
		}
		if err := insertCheckpointRecoveryRequest(ctx, q, request, owned.ActiveResourceID); err != nil {
			return err
		}
		if _, err := signalSessionExecutionWakeTx(ctx, tx, queue, request.SessionID, "recovery:"+request.ID); err != nil {
			return err
		}
		row, err := q.GetCheckpointRecovery(ctx, request.ID)
		if err == nil {
			result = recoveryReceipt(row)
		}
		return err
	})
	return result, err
}

func replayCheckpointRecovery(ctx context.Context, q *dbsql.Queries, request persistence.RecoveryRequest) (persistence.RecoveryReceipt, error) {
	row, err := q.GetCheckpointRecovery(ctx, request.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.RecoveryReceipt{}, nil
	}
	if err != nil {
		return persistence.RecoveryReceipt{}, err
	}
	receipt := recoveryReceipt(row)
	if receipt.RecoveryRequest != request {
		return persistence.RecoveryReceipt{}, fmt.Errorf("recovery identity already has different input")
	}
	return receipt, nil
}

func authorizeCheckpointRecoveryRequest(ctx context.Context, q *dbsql.Queries, request persistence.RecoveryRequest) (dbsql.GetSandboxRow, error) {
	session, err := q.GetSession(ctx, request.SessionID)
	if err != nil {
		return dbsql.GetSandboxRow{}, err
	}
	checkpoint, err := q.GetSandboxCheckpointByReference(ctx, dbsql.GetSandboxCheckpointByReferenceParams{
		Repository: request.Repository, SnapshotID: request.SnapshotID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsql.GetSandboxRow{}, persistence.ErrCheckpointNotFound
	}
	if err != nil {
		return dbsql.GetSandboxRow{}, err
	}
	if checkpoint.SessionID != request.SessionID || checkpoint.SandboxID != request.SandboxID {
		return dbsql.GetSandboxRow{}, fmt.Errorf("checkpoint belongs to a different Session Sandbox")
	}
	if checkpoint.ProfileName != session.SandboxProfile || checkpoint.ProfileRevision != session.SandboxProfileRevision {
		return dbsql.GetSandboxRow{}, fmt.Errorf("checkpoint requires a different Sandbox profile revision")
	}
	owned, err := q.GetSandbox(ctx, request.SandboxID)
	if err == nil && owned.ProviderID == "" {
		return dbsql.GetSandboxRow{}, fmt.Errorf("checkpoint recovery requires retained source custody")
	}
	return owned, err
}

func insertCheckpointRecoveryRequest(ctx context.Context, q *dbsql.Queries, request persistence.RecoveryRequest, sourceResourceID string) error {
	if err := q.InsertCheckpointRecoveryHold(ctx, dbsql.InsertCheckpointRecoveryHoldParams{ID: request.ID, SandboxID: request.SandboxID}); err != nil {
		return err
	}
	resourceID := request.ID + ":replacement"
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	if err := q.ReserveSandboxResource(ctx, dbsql.ReserveSandboxResourceParams{
		ID: resourceID, SandboxID: request.SandboxID, OwnershipNonce: hex.EncodeToString(nonce[:]),
	}); err != nil {
		return err
	}
	return q.InsertCheckpointRecovery(ctx, dbsql.InsertCheckpointRecoveryParams{
		ID: request.ID, SandboxID: request.SandboxID, CheckpointRepository: request.Repository,
		CheckpointSnapshotID: request.SnapshotID, SourceResourceID: sourceResourceID, DestinationResourceID: resourceID,
	})
}

func (s Store) SessionRecoveries(ctx context.Context, sessionID string) ([]persistence.RecoveryReceipt, error) {
	rows, err := dbsql.New(s.DB).ListSessionCheckpointRecoveries(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	receipts := make([]persistence.RecoveryReceipt, 0, len(rows))
	for _, row := range rows {
		receipts = append(receipts, recoveryReceipt(dbsql.GetCheckpointRecoveryRow(row)))
	}
	return receipts, nil
}

func recoveryReceipt(row dbsql.GetCheckpointRecoveryRow) persistence.RecoveryReceipt {
	checkpoint := checkpointFromValues(
		row.SessionID, row.SandboxID, row.CheckpointResourceID, row.ProfileName, row.ProfileRevision,
		row.EffectiveUpgradeID, row.LastActivityAt, row.MessageSequence, row.CompletedTurnSequence,
		row.DeliveryHoldCount, row.Cleanup, row.CheckpointRepository, row.CheckpointSnapshotID, row.PublishedAt,
	)
	return persistence.RecoveryReceipt{
		RecoveryRequest: persistence.RecoveryRequest{
			ID: row.ID, SessionID: row.SessionID, SandboxID: row.SandboxID,
			Repository: row.CheckpointRepository, SnapshotID: row.CheckpointSnapshotID,
		},
		Checkpoint: checkpoint, SourceResourceID: row.SourceResourceID, SourceProviderID: row.SourceProviderID,
		DestinationResourceID: row.DestinationResourceID, DestinationProviderID: row.DestinationProviderID,
		Package:     persistence.EffectivePackage{UpgradeID: row.EffectiveUpgradeID, PackagePath: row.PackagePath, Version: row.PackageVersion},
		RequestedAt: row.RequestedAt, VerifiedAt: timeValue(row.VerifiedAt),
		SourceDeletedAt: timeValue(row.SourceDeletedAt), DestinationDeletedAt: timeValue(row.DestinationDeletedAt), FinishedAt: timeValue(row.FinishedAt),
	}
}

func (s Store) RecoveryNativeStateSafe(ctx context.Context, receipt persistence.RecoveryReceipt) (bool, error) {
	if receipt.SandboxID == "" || receipt.Checkpoint.MessageSequence < 0 {
		return false, fmt.Errorf("recovery safety requires an exact checkpoint boundary")
	}
	safe, err := dbsql.New(s.DB).RecoveryNativeStateSafe(ctx, dbsql.RecoveryNativeStateSafeParams{
		SandboxID: receipt.SandboxID, MessageSequence: receipt.Checkpoint.MessageSequence,
	})
	return safe.Valid && safe.Bool, err
}

// RecordRecoveryRestored binds the replacement only after exact restore succeeds.
// The resource binding itself is the restore receipt; caller holds the Session fence.
func (s Store) RecordRecoveryRestored(ctx context.Context, receipt persistence.RecoveryReceipt, providerID string) error {
	if providerID == "" || providerID != strings.TrimSpace(providerID) {
		return fmt.Errorf("recovery omitted provider resource identity")
	}
	return expectOneRows(dbsql.New(s.DB).BindRecoveredResource(ctx, dbsql.BindRecoveredResourceParams{
		ResourceID: receipt.DestinationResourceID, ProviderID: providerID,
	}))
}

func (s Store) RecordRecoveryVerified(ctx context.Context, id string) error {
	return expectOneRows(dbsql.New(s.DB).RecordRecoveryVerified(ctx, id))
}

// AbandonCheckpointRecoveryForCleanup releases the recovery hold only after
// scheduled cleanup has removed its replacement. Caller holds the Session fence.
func (s Store) AbandonCheckpointRecoveryForCleanup(ctx context.Context, receipt persistence.RecoveryReceipt) error {
	q := dbsql.New(s.DB)
	quiet, err := q.UpgradeQuiescent(ctx, receipt.SandboxID)
	if err != nil {
		return err
	}
	if !quiet {
		return fmt.Errorf("native delivery is not quiescent")
	}
	return expectOneRows(q.AbandonCheckpointRecovery(ctx, receipt.ID))
}

// FinishCheckpointRecovery atomically adopts verified custody, releases its
// exact hold, and wakes queued delivery. Caller holds the Session effect fence.
func (s Store) FinishCheckpointRecovery(ctx context.Context, queue string, expected persistence.RecoveryReceipt) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := dbsql.New(tx)
	session, err := q.GetSessionAdmissionForUpdate(ctx, expected.SessionID)
	if err != nil {
		return err
	}
	if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
		return fmt.Errorf("recovery release requires open admission")
	}
	fullSession, err := q.GetSession(ctx, expected.SessionID)
	if err != nil {
		return err
	}
	row, err := q.GetCheckpointRecovery(ctx, expected.ID)
	if err != nil {
		return err
	}
	receipt := recoveryReceipt(row)
	if receipt.RecoveryRequest != expected.RecoveryRequest || receipt.SourceResourceID != expected.SourceResourceID || receipt.DestinationResourceID != expected.DestinationResourceID {
		return fmt.Errorf("recovery custody changed")
	}
	if !receipt.FinishedAt.IsZero() {
		return nil
	}
	if err := authorizeRecoveryRelease(ctx, q, fullSession.SandboxProfile, fullSession.SandboxProfileRevision, receipt); err != nil {
		return err
	}
	if err := switchAndReleaseRecovery(ctx, tx, q, queue, receipt); err != nil {
		return err
	}
	return tx.Commit()
}

func switchAndReleaseRecovery(ctx context.Context, tx *sql.Tx, q *dbsql.Queries, queue string, receipt persistence.RecoveryReceipt) error {
	if err := expectOneRows(q.SwitchSandboxResource(ctx, dbsql.SwitchSandboxResourceParams{
		SandboxID: receipt.SandboxID, SourceResourceID: receipt.SourceResourceID,
		DestinationResourceID: receipt.DestinationResourceID,
	})); err != nil {
		return err
	}
	if err := expectOneRows(q.RecordCheckpointRecoveryFinished(ctx, receipt.ID)); err != nil {
		return err
	}
	if err := expectOneRows(q.ReleaseSandboxDeliveryHold(ctx, dbsql.ReleaseSandboxDeliveryHoldParams{ID: receipt.ID, SandboxID: receipt.SandboxID})); err != nil {
		return err
	}
	if _, err := signalSessionExecutionWakeTx(ctx, tx, queue, receipt.SessionID, "recovery-finished:"+receipt.ID); err != nil {
		return err
	}
	return nil
}

func authorizeRecoveryRelease(ctx context.Context, q *dbsql.Queries, profileName, profileRevision string, receipt persistence.RecoveryReceipt) error {
	hold, err := q.GetSandboxDeliveryHold(ctx, receipt.ID)
	if err != nil {
		return err
	}
	if hold.SandboxID != receipt.SandboxID || hold.Reason != "checkpoint_recovery" || hold.ReleasedAt.Valid || receipt.VerifiedAt.IsZero() || receipt.SourceDeletedAt.IsZero() {
		return fmt.Errorf("recovery requires its held verified receipt")
	}
	if receipt.Checkpoint.ProfileName != profileName || receipt.Checkpoint.ProfileRevision != profileRevision {
		return fmt.Errorf("recovery profile revision changed")
	}
	safe, err := q.RecoveryNativeStateSafe(ctx, dbsql.RecoveryNativeStateSafeParams{
		SandboxID: receipt.SandboxID, MessageSequence: receipt.Checkpoint.MessageSequence,
	})
	if err != nil {
		return err
	}
	if !safe.Valid || !safe.Bool {
		return fmt.Errorf("native work exists beyond the selected checkpoint")
	}
	if receipt.DestinationProviderID == "" || !receipt.DestinationDeletedAt.IsZero() {
		return fmt.Errorf("recovery destination is not available")
	}
	return nil
}
