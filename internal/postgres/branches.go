package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

// RequestCheckpointBranch atomically admits a new Session, reserves its own
// resource and repository namespace, and holds it before the lifecycle task
// can create compute. The source is only read; its current revision may have
// advanced beyond the selected immutable checkpoint.
func (s Store) RequestCheckpointBranch(ctx context.Context, queue string, request persistence.BranchRequest) (persistence.BranchReceipt, error) {
	if err := request.Validate(); err != nil {
		return persistence.BranchReceipt{}, err
	}
	destinationID := core.SessionID("checkpoint-branch:" + request.ID)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	defer tx.Rollback()
	q := dbsql.New(tx)
	_, err = q.GetAdmittedSessionForUpdate(ctx, "checkpoint-branch:"+request.ID)
	if err == nil {
		return replayCheckpointBranch(ctx, tx, q, request, destinationID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return persistence.BranchReceipt{}, err
	}
	source, checkpoint, err := authorizeBranchSource(ctx, q, request)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	inserted, err := q.InsertAdmittedSession(ctx, dbsql.InsertAdmittedSessionParams{
		ID: destinationID, AdmissionKey: "checkpoint-branch:" + request.ID,
		AgentsMd: source.AgentsMd, SandboxProfile: source.SandboxProfile,
		SandboxProfileRevision: source.SandboxProfileRevision,
		ProviderConnection:     source.ProviderConnection, Model: source.Model,
		ReasoningEffort: source.ReasoningEffort, KeepRunning: source.KeepRunning,
	})
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	if inserted == 0 {
		return replayCheckpointBranch(ctx, tx, q, request, destinationID)
	}
	if err := persistNewCheckpointBranch(ctx, tx, q, queue, request, destinationID, source.ThreadID, checkpoint.NativeRevision); err != nil {
		return persistence.BranchReceipt{}, err
	}
	result, err := checkpointBranchReceipt(ctx, q, request.ID)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	return result, tx.Commit()
}

func replayCheckpointBranch(ctx context.Context, tx *sql.Tx, q *dbsql.Queries, request persistence.BranchRequest, destinationID string) (persistence.BranchReceipt, error) {
	replayed, err := checkpointBranchReceipt(ctx, q, request.ID)
	if err != nil || replayed.BranchRequest != request || replayed.DestinationSessionID != destinationID {
		return persistence.BranchReceipt{}, fmt.Errorf("%w: branch identity already belongs to another request", persistence.ErrCaptureConflict)
	}
	return replayed, tx.Commit()
}

func authorizeBranchSource(ctx context.Context, q *dbsql.Queries, request persistence.BranchRequest) (dbsql.GetSessionRow, dbsql.GetSandboxCheckpointByReferenceRow, error) {
	source, err := q.GetSession(ctx, request.SourceSessionID)
	if err != nil {
		return dbsql.GetSessionRow{}, dbsql.GetSandboxCheckpointByReferenceRow{}, err
	}
	checkpoint, err := q.GetSandboxCheckpointByReference(ctx, dbsql.GetSandboxCheckpointByReferenceParams{Repository: request.Repository, SnapshotID: request.SnapshotID})
	if errors.Is(err, sql.ErrNoRows) {
		return dbsql.GetSessionRow{}, dbsql.GetSandboxCheckpointByReferenceRow{}, persistence.ErrCheckpointNotFound
	}
	if err != nil {
		return dbsql.GetSessionRow{}, dbsql.GetSandboxCheckpointByReferenceRow{}, err
	}
	if checkpoint.SessionID != source.ID || checkpoint.SandboxID != core.MainSandboxName(source.ID) ||
		checkpoint.ProfileName != source.SandboxProfile || checkpoint.ProfileRevision != source.SandboxProfileRevision ||
		checkpoint.NativeRevision == 0 || source.ThreadID == "" {
		return dbsql.GetSessionRow{}, dbsql.GetSandboxCheckpointByReferenceRow{}, fmt.Errorf("%w: source checkpoint cannot bind a supported native Thread", persistence.ErrCaptureConflict)
	}
	if checkpoint.EffectiveUpgradeID.Valid {
		return dbsql.GetSessionRow{}, dbsql.GetSandboxCheckpointByReferenceRow{}, fmt.Errorf("%w: branching an activated package generation is not yet supported", persistence.ErrCaptureConflict)
	}
	return source, checkpoint, nil
}

func persistNewCheckpointBranch(ctx context.Context, tx *sql.Tx, q *dbsql.Queries, queue string, request persistence.BranchRequest, destinationID, threadID string, nativeRevision int64) error {
	destinationSandboxID := core.MainSandboxName(destinationID)
	if err := reserveAdmittedSandbox(ctx, q, destinationID, destinationSandboxID); err != nil {
		return err
	}
	if err := expectOneRows(q.BindCheckpointBranchThread(ctx, dbsql.BindCheckpointBranchThreadParams{
		SessionID: destinationID, ThreadID: nullableString(threadID), NativeRevision: nativeRevision,
	})); err != nil {
		return err
	}
	if err := q.InsertCheckpointBranchHold(ctx, dbsql.InsertCheckpointBranchHoldParams{ID: request.ID, DestinationSandboxID: destinationSandboxID}); err != nil {
		return err
	}
	if err := q.InsertCheckpointBranch(ctx, dbsql.InsertCheckpointBranchParams{
		ID: request.ID, SourceSessionID: request.SourceSessionID, DestinationSessionID: destinationID,
		DestinationSandboxID: destinationSandboxID, CheckpointRepository: request.Repository,
		CheckpointSnapshotID: request.SnapshotID, ThreadID: threadID,
	}); err != nil {
		return err
	}
	if err := scheduleSessionTaskTx(ctx, tx, queue, destinationID, direct.TaskName, direct.TaskKey(destinationID)); err != nil {
		return err
	}
	return nil
}

func (s Store) CheckpointBranch(ctx context.Context, id string) (persistence.BranchReceipt, error) {
	return checkpointBranchReceipt(ctx, dbsql.New(s.DB), id)
}

func (s Store) SessionCheckpointBranch(ctx context.Context, sessionID string) (persistence.BranchReceipt, bool, error) {
	row, err := dbsql.New(s.DB).GetCheckpointBranchBySession(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.BranchReceipt{}, false, nil
	}
	if err != nil {
		return persistence.BranchReceipt{}, false, err
	}
	receipt, err := checkpointBranchReceipt(ctx, dbsql.New(s.DB), row.ID)
	return receipt, true, err
}

func (s Store) HasCheckpointBranch(ctx context.Context, sessionID string) (bool, error) {
	_, err := dbsql.New(s.DB).GetCheckpointBranchBySession(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func checkpointBranchReceipt(ctx context.Context, q *dbsql.Queries, id string) (persistence.BranchReceipt, error) {
	row, err := q.GetCheckpointBranchByID(ctx, id)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	checkpointRow, err := q.GetSandboxCheckpointByReference(ctx, dbsql.GetSandboxCheckpointByReferenceParams{
		Repository: row.CheckpointRepository, SnapshotID: row.CheckpointSnapshotID,
	})
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	checkpoint := checkpointFromValues(checkpointRow.SessionID, checkpointRow.SandboxID, checkpointRow.ResourceID,
		checkpointRow.ProfileName, checkpointRow.ProfileRevision, checkpointRow.EffectiveUpgradeID.String,
		checkpointRow.LastActivityAt, checkpointRow.NativeRevision, checkpointRow.DeliveryHoldCount,
		checkpointRow.Cleanup, checkpointRow.Repository, checkpointRow.SnapshotID, checkpointRow.PublishedAt)
	return persistence.BranchReceipt{
		BranchRequest: persistence.BranchRequest{ID: row.ID, SourceSessionID: row.SourceSessionID,
			Repository: row.CheckpointRepository, SnapshotID: row.CheckpointSnapshotID},
		DestinationSessionID: row.DestinationSessionID, DestinationSandboxID: row.DestinationSandboxID,
		Checkpoint: checkpoint, ThreadID: row.ThreadID, RequestedAt: row.RequestedAt,
		RestoredAt: timeValue(row.RestoredAt), ReleaseRequestedAt: timeValue(row.ReleaseRequestedAt),
		ReadyAt: timeValue(row.ReadyAt),
	}, nil
}

func (s Store) RecordCheckpointBranchRestored(ctx context.Context, id string) error {
	return expectOneRows(dbsql.New(s.DB).RecordCheckpointBranchRestored(ctx, id))
}

func (s Store) CheckpointBranchFilePreparationAllowed(ctx context.Context, sandboxID string) (bool, error) {
	return dbsql.New(s.DB).CheckpointBranchFilePreparationAllowed(ctx, sandboxID)
}

// ReleaseCheckpointBranch requests fresh route installation and native resume.
// The hold survives this request and is removed only after verification.
func (s Store) ReleaseCheckpointBranch(ctx context.Context, queue, id string) (persistence.BranchReceipt, error) {
	receipt, err := s.CheckpointBranch(ctx, id)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	if receipt.RestoredAt.IsZero() {
		return persistence.BranchReceipt{}, fmt.Errorf("%w: branch has not restored its checkpoint", persistence.ErrCaptureConflict)
	}
	err = s.WithSessionFence(ctx, receipt.DestinationSessionID, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		q := dbsql.New(tx)
		session, err := q.GetSessionAdmissionForUpdate(ctx, receipt.DestinationSessionID)
		if err != nil {
			return err
		}
		if !session.AdmissionOpen || session.CleanupState != core.CleanupPending || session.ThreadID != receipt.ThreadID {
			return fmt.Errorf("%w: branch Session is not open with its exact native binding", persistence.ErrCaptureConflict)
		}
		if err := expectOneRows(q.RequestCheckpointBranchRelease(ctx, id)); err != nil {
			return err
		}
		if _, err := signalSessionExecutionWakeTx(ctx, tx, queue, receipt.DestinationSessionID, "branch-release:"+id); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	return s.CheckpointBranch(ctx, id)
}

func (s Store) FinishCheckpointBranch(ctx context.Context, receipt persistence.BranchReceipt) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := dbsql.New(tx)
	current, err := checkpointBranchReceipt(ctx, q, receipt.ID)
	if err != nil {
		return err
	}
	if current.BranchRequest != receipt.BranchRequest || current.DestinationSessionID != receipt.DestinationSessionID ||
		current.DestinationSandboxID != receipt.DestinationSandboxID || current.Checkpoint != receipt.Checkpoint {
		return fmt.Errorf("branch custody changed")
	}
	if !current.ReadyAt.IsZero() {
		return nil
	}
	if current.ReleaseRequestedAt.IsZero() {
		return fmt.Errorf("branch release was not requested")
	}
	owner, err := q.GetSandbox(ctx, current.DestinationSandboxID)
	if err != nil {
		return err
	}
	if owner.SessionID != current.DestinationSessionID || owner.ProviderID == "" {
		return fmt.Errorf("branch destination resource is unavailable")
	}
	route, err := q.GetScopedAction(ctx, dbsql.GetScopedActionParams{
		SessionID: current.DestinationSessionID, Kind: core.ActionRouteCreate, ScopeKey: current.DestinationSandboxID,
	})
	if err != nil || route.State != core.ActionSucceeded {
		return fmt.Errorf("branch destination route is not installed")
	}
	if err := expectOneRows(q.FinishCheckpointBranch(ctx, receipt.ID)); err != nil {
		return err
	}
	return tx.Commit()
}
