package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/persistence"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
)

func (s Store) Boundary(ctx context.Context, sandboxID string, cleanup bool) (persistence.CaptureBoundary, error) {
	if !exactCheckpointIdentity(sandboxID) {
		return persistence.CaptureBoundary{}, fmt.Errorf("checkpoint boundary requires an exact bounded Sandbox ID")
	}
	row, err := dbsql.New(s.DB).GetCheckpointBoundary(ctx, dbsql.GetCheckpointBoundaryParams{SandboxID: sandboxID, Cleanup: cleanup})
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.CaptureBoundary{}, ErrNotFound
	}
	if err != nil {
		return persistence.CaptureBoundary{}, err
	}
	return captureBoundary(
		row.JobID, row.SandboxID, row.ResourceID, row.ProfileName, row.ProfileRevision,
		row.EffectiveUpgradeID, row.LastActivityAt, row.MessageSequence,
		row.CompletedTurnSequence, row.DeliveryHoldCount, cleanup, row.Eligible,
	), nil
}

// PublishCheckpoint records a successful upload only after the exact execution
// boundary remains current. Hashing and upload happen before this short fence.
// The Job row lock serializes the recheck with Message admission, which does not
// take the external-effect fence.
func (s Store) PublishCheckpoint(ctx context.Context, expected persistence.CaptureBoundary, reference persistence.Reference) (persistence.Checkpoint, error) {
	if err := validateCheckpoint(expected, reference); err != nil {
		return persistence.Checkpoint{}, err
	}
	var checkpoint persistence.Checkpoint
	err := s.WithJobFence(ctx, expected.JobID, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		q := dbsql.New(tx)
		if _, err := q.GetJobAdmissionForUpdate(ctx, expected.JobID); err != nil {
			return err
		}
		stored, err := q.GetSandboxCheckpointByReference(ctx, dbsql.GetSandboxCheckpointByReferenceParams{
			Repository: reference.Repository, SnapshotID: reference.SnapshotID,
		})
		if err == nil {
			checkpoint = checkpointFromValues(
				stored.JobID, stored.SandboxID, stored.ResourceID, stored.ProfileName,
				stored.ProfileRevision, stored.EffectiveUpgradeID.String, stored.LastActivityAt,
				stored.MessageSequence, stored.CompletedTurnSequence, stored.DeliveryHoldCount,
				stored.Cleanup, stored.Repository, stored.SnapshotID, stored.PublishedAt,
			)
			if checkpoint.CaptureBoundary != expected || checkpoint.Reference != reference {
				return fmt.Errorf("checkpoint reference already records a different execution boundary")
			}
			return tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row, err := q.GetCheckpointBoundary(ctx, dbsql.GetCheckpointBoundaryParams{SandboxID: expected.SandboxID, Cleanup: expected.Cleanup})
		if err != nil {
			return err
		}
		current := captureBoundary(
			row.JobID, row.SandboxID, row.ResourceID, row.ProfileName, row.ProfileRevision,
			row.EffectiveUpgradeID, row.LastActivityAt, row.MessageSequence,
			row.CompletedTurnSequence, row.DeliveryHoldCount, expected.Cleanup, row.Eligible,
		)
		if current != expected {
			return persistence.ErrCheckpointSuperseded
		}
		if !current.Eligible {
			return persistence.ErrCheckpointIneligible
		}
		_, err = q.InsertSandboxCheckpoint(ctx, dbsql.InsertSandboxCheckpointParams{
			Repository: reference.Repository, SnapshotID: reference.SnapshotID,
			SandboxID: expected.SandboxID, ResourceID: expected.ResourceID,
			ProfileName: expected.ProfileName, ProfileRevision: expected.ProfileRevision,
			EffectiveUpgradeID: expected.EffectiveUpgradeID, LastActivityAt: expected.LastActivityAt,
			MessageSequence: expected.MessageSequence, CompletedTurnSequence: expected.CompletedTurnSequence,
			DeliveryHoldCount: expected.DeliveryHoldCount, Cleanup: expected.Cleanup,
		})
		if err != nil {
			return err
		}
		stored, err = q.GetSandboxCheckpointByReference(ctx, dbsql.GetSandboxCheckpointByReferenceParams{
			Repository: reference.Repository, SnapshotID: reference.SnapshotID,
		})
		if err != nil {
			return err
		}
		checkpoint = checkpointFromValues(
			stored.JobID, stored.SandboxID, stored.ResourceID, stored.ProfileName,
			stored.ProfileRevision, stored.EffectiveUpgradeID.String, stored.LastActivityAt,
			stored.MessageSequence, stored.CompletedTurnSequence, stored.DeliveryHoldCount,
			stored.Cleanup, stored.Repository, stored.SnapshotID, stored.PublishedAt,
		)
		if checkpoint.CaptureBoundary != expected || checkpoint.Reference != reference {
			return fmt.Errorf("checkpoint reference already records a different execution boundary")
		}
		return tx.Commit()
	})
	return checkpoint, err
}

func (s Store) LastCheckpoint(ctx context.Context, sandboxID string) (persistence.Checkpoint, error) {
	if !exactCheckpointIdentity(sandboxID) {
		return persistence.Checkpoint{}, fmt.Errorf("last checkpoint requires an exact bounded Sandbox ID")
	}
	row, err := dbsql.New(s.DB).GetLastSandboxCheckpoint(ctx, sandboxID)
	if errors.Is(err, sql.ErrNoRows) {
		return persistence.Checkpoint{}, persistence.ErrCheckpointNotFound
	}
	if err != nil {
		return persistence.Checkpoint{}, err
	}
	return checkpointFromValues(
		row.JobID, row.SandboxID, row.ResourceID, row.ProfileName, row.ProfileRevision,
		row.EffectiveUpgradeID.String, row.LastActivityAt, row.MessageSequence,
		row.CompletedTurnSequence, row.DeliveryHoldCount, row.Cleanup,
		row.Repository, row.SnapshotID, row.PublishedAt,
	), nil
}

func (s Store) ListCheckpoints(ctx context.Context, sandboxID string) ([]persistence.Checkpoint, error) {
	if !exactCheckpointIdentity(sandboxID) {
		return nil, fmt.Errorf("checkpoint history requires an exact bounded Sandbox ID")
	}
	rows, err := dbsql.New(s.DB).ListSandboxCheckpoints(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	checkpoints := make([]persistence.Checkpoint, 0, len(rows))
	for _, row := range rows {
		checkpoints = append(checkpoints, checkpointFromValues(
			row.JobID, row.SandboxID, row.ResourceID, row.ProfileName, row.ProfileRevision,
			row.EffectiveUpgradeID.String, row.LastActivityAt, row.MessageSequence,
			row.CompletedTurnSequence, row.DeliveryHoldCount, row.Cleanup,
			row.Repository, row.SnapshotID, row.PublishedAt,
		))
	}
	return checkpoints, nil
}

// ListCheckpointCandidates returns a bounded scan of idle, settled Sandboxes
// whose current execution boundary has not already been published.
func (s Store) ListCheckpointCandidates(ctx context.Context, idleFor time.Duration) ([]persistence.CaptureBoundary, error) {
	if idleFor < 0 {
		return nil, fmt.Errorf("checkpoint idle duration cannot be negative")
	}
	ids, err := dbsql.New(s.DB).ListIdleCheckpointSandboxIDs(ctx, idleFor.Seconds())
	if err != nil {
		return nil, err
	}
	candidates := make([]persistence.CaptureBoundary, 0, len(ids))
	for _, sandboxID := range ids {
		boundary, err := s.Boundary(ctx, sandboxID, false)
		if err != nil {
			return nil, err
		}
		if !boundary.Eligible || boundary.CompletedTurnSequence == 0 || boundary.LastActivityAt.After(time.Now().Add(-idleFor)) {
			continue
		}
		last, err := s.LastCheckpoint(ctx, sandboxID)
		if errors.Is(err, persistence.ErrCheckpointNotFound) {
			candidates = append(candidates, boundary)
			continue
		}
		if err != nil {
			return nil, err
		}
		if last.CaptureBoundary != boundary {
			candidates = append(candidates, boundary)
		}
	}
	return candidates, nil
}

func captureBoundary(jobID, sandboxID, resourceID, profileName, profileRevision, effectiveUpgradeID string, lastActivityAt time.Time, messageSequence, completedTurnSequence, deliveryHoldCount int64, cleanup, eligible bool) persistence.CaptureBoundary {
	return persistence.CaptureBoundary{
		JobID: jobID, SandboxID: sandboxID, ResourceID: resourceID,
		ProfileName: profileName, ProfileRevision: profileRevision,
		EffectiveUpgradeID: effectiveUpgradeID, LastActivityAt: lastActivityAt,
		MessageSequence: messageSequence, CompletedTurnSequence: completedTurnSequence,
		DeliveryHoldCount: deliveryHoldCount, Cleanup: cleanup, Eligible: eligible,
	}
}

func checkpointFromValues(jobID, sandboxID, resourceID, profileName, profileRevision, effectiveUpgradeID string, lastActivityAt time.Time, messageSequence, completedTurnSequence, deliveryHoldCount int64, cleanup bool, repository, snapshotID string, publishedAt time.Time) persistence.Checkpoint {
	return persistence.Checkpoint{
		CaptureBoundary: captureBoundary(
			jobID, sandboxID, resourceID, profileName, profileRevision, effectiveUpgradeID,
			lastActivityAt, messageSequence, completedTurnSequence, deliveryHoldCount, cleanup, true,
		),
		Reference:   persistence.Reference{Repository: repository, SnapshotID: snapshotID},
		PublishedAt: publishedAt,
	}
}

func validateCheckpoint(boundary persistence.CaptureBoundary, reference persistence.Reference) error {
	for _, id := range []string{
		boundary.JobID, boundary.SandboxID, boundary.ResourceID,
		boundary.ProfileName, boundary.ProfileRevision,
	} {
		if !exactCheckpointIdentity(id) {
			return fmt.Errorf("checkpoint publication requires exact bounded custody identities")
		}
	}
	if boundary.EffectiveUpgradeID != "" && !exactCheckpointIdentity(boundary.EffectiveUpgradeID) {
		return fmt.Errorf("checkpoint publication has an invalid package generation reference")
	}
	if boundary.LastActivityAt.IsZero() || boundary.MessageSequence < 0 ||
		boundary.CompletedTurnSequence < 0 || boundary.CompletedTurnSequence > boundary.MessageSequence ||
		boundary.DeliveryHoldCount < 0 || !boundary.Eligible {
		return persistence.ErrCheckpointIneligible
	}
	if !exactCheckpointIdentity(reference.Repository) || !sha256Digest.MatchString(reference.SnapshotID) {
		return fmt.Errorf("checkpoint publication requires a logical repository and full snapshot identity")
	}
	return nil
}

func exactCheckpointIdentity(value string) bool {
	return value != "" && len(value) <= 256 && value == strings.TrimSpace(value)
}
