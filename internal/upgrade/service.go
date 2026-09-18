package upgrade

import (
	"context"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
	"github.com/aphronio/dorf/internal/telemetry"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

type Store interface {
	WithSessionFence(context.Context, string, func() error) error
	Session(context.Context, string) (core.Session, error)
	Sandbox(context.Context, string) (core.Sandbox, error)
	SandboxResources(context.Context, string) ([]core.SandboxResource, error)
	SessionDeliveryHolds(context.Context, string) ([]core.SandboxDeliveryHold, error)
	Deliveries(context.Context, string) ([]core.Delivery, error)
	SessionUpgrades(context.Context, string) ([]Receipt, error)
	UpgradeQuiescent(context.Context, string) (bool, error)
	SandboxResource(context.Context, string, string, string) (core.Sandbox, error)
	RecordUpgradePreparation(context.Context, string, string) error
	RecordUpgradeQuiesced(context.Context, string) error
	RecordUpgradeCheckpoint(context.Context, string, provider.Checkpoint) error
	RecordUpgradeActivated(context.Context, string) error
	RequestUpgradeRollback(context.Context, string, string) error
	ReserveUpgradeDestination(context.Context, Receipt, bool) error
	RecordUpgradeRestored(context.Context, Receipt, string) error
	RecordUpgradeVerified(context.Context, string) error
	RecordSandboxResourceDeleted(context.Context, core.Sandbox) error
	RecordUpgradeCheckpointDeleted(context.Context, string) error
	FinishSandboxUpgrade(context.Context, string, Receipt) error
	SetExecutionAttention(context.Context, string, string, string) error
	ClearExecutionAttention(context.Context, string, string) error
}

type Service struct {
	Store    Store
	Driver   Driver
	Queue    string
	Provider string
	Claim    func(context.Context) error
	Emit     func(telemetry.Event)
}

// Reconcile runs one effect under the same Session fence as native delivery and
// cleanup. Pending follows are excluded from quiescence; pre-hold work drains
// through the ordinary executor. The existing direct task owns retries.
func (s Service) Reconcile(ctx context.Context, sessionID string) (bool, error) {
	var progress bool
	err := s.Store.WithSessionFence(ctx, sessionID, func() error {
		session, err := s.Store.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if task, ok := absurd.TaskFromContext(ctx); ok && task.TaskID() != session.CurrentTaskID {
			return fmt.Errorf("upgrade executor no longer owns the Session task")
		}
		if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
			return nil
		}
		receipts, err := s.Store.SessionUpgrades(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, receipt := range receipts {
			if !receipt.FinishedAt.IsZero() {
				continue
			}
			if err := s.authorize(ctx, receipt); err != nil {
				return err
			}
			quiet, err := s.Store.UpgradeQuiescent(ctx, receipt.SandboxID)
			if err != nil || !quiet {
				return err
			}
			if err := s.requireClaim(ctx); err != nil {
				return err
			}
			err = s.step(ctx, receipt)
			progress = err == nil
			return err
		}
		return nil
	})
	return progress, err
}

func (s Service) authorize(ctx context.Context, r Receipt) error {
	owned, err := s.Store.Sandbox(ctx, r.SandboxID)
	if err != nil {
		return err
	}
	if owned.SessionID != r.SessionID || owned.ResourceID != r.SourceResourceID {
		return fmt.Errorf("upgrade source binding was superseded")
	}
	holds, err := s.Store.SessionDeliveryHolds(ctx, r.SessionID)
	if err != nil {
		return err
	}
	for _, hold := range holds {
		if hold.ID == r.ID && hold.SandboxID == r.SandboxID {
			return nil
		}
	}
	return fmt.Errorf("upgrade lost its exact delivery hold")
}

func (s Service) step(ctx context.Context, r Receipt) error {
	source, err := s.Store.SandboxResource(ctx, r.SessionID, r.SandboxID, r.SourceResourceID)
	if err != nil {
		return err
	}
	switch {
	case r.PreviousVersion == "":
		return s.prepare(ctx, r, source)
	case r.QuiescedAt.IsZero():
		return s.perform(ctx, r, "quiesce", func() error {
			runs, err := s.boundRuns(ctx, r)
			if err != nil {
				return err
			}
			if err := s.Driver.Quiesce(ctx, source, runs); err != nil {
				return err
			}
			return s.record(ctx, func() error { return s.Store.RecordUpgradeQuiesced(ctx, r.ID) })
		})
	case r.Checkpoint.Reference == "":
		return s.perform(ctx, r, "checkpoint", func() error {
			checkpoint, err := s.Driver.Capture(ctx, source, r.ID)
			if err != nil {
				return err
			}
			if checkpoint.SourceID != source.ProviderID {
				return fmt.Errorf("checkpoint source differs from attested resource")
			}
			return s.record(ctx, func() error { return s.Store.RecordUpgradeCheckpoint(ctx, r.ID, checkpoint) })
		})
	case !r.RollbackAt.IsZero() && r.RestoredAt.IsZero():
		return s.restore(ctx, r, source)
	case r.ActivatedAt.IsZero() && r.RollbackAt.IsZero():
		return s.activate(ctx, r, source)
	case r.VerifiedAt.IsZero():
		return s.verify(ctx, r, source)
	default:
		return s.finish(ctx, r, source)
	}
}

func (s Service) activate(ctx context.Context, r Receipt, source core.Sandbox) error {
	return s.perform(ctx, r, "activate", func() error {
		if err := s.Driver.Activate(ctx, source, r.Request); err != nil {
			s.event(r, "activate.failed", true, 0)
			return s.record(ctx, func() error { return s.Store.RequestUpgradeRollback(ctx, r.ID, "activation_failed") })
		}
		return s.record(ctx, func() error { return s.Store.RecordUpgradeActivated(ctx, r.ID) })
	})
}

func (s Service) restore(ctx context.Context, r Receipt, source core.Sandbox) error {
	if r.DestinationResourceID == "" {
		return s.perform(ctx, r, "reserve-recovery", func() error {
			return s.record(ctx, func() error { return s.Store.ReserveUpgradeDestination(ctx, r, s.Driver.ReplacesResource()) })
		})
	}
	destination, err := s.Store.SandboxResource(ctx, r.SessionID, r.SandboxID, r.DestinationResourceID)
	if err != nil {
		return err
	}
	return s.perform(ctx, r, "restore", func() error {
		id, err := s.Driver.Restore(ctx, source, destination, r.Checkpoint)
		if err != nil {
			return err
		}
		return s.record(ctx, func() error { return s.Store.RecordUpgradeRestored(ctx, r, id) })
	})
}

func (s Service) verify(ctx context.Context, r Receipt, source core.Sandbox) error {
	destination, version := source, r.Version
	if !r.RollbackAt.IsZero() {
		var err error
		destination, err = s.Store.SandboxResource(ctx, r.SessionID, r.SandboxID, r.DestinationResourceID)
		if err != nil {
			return err
		}
		version = r.PreviousVersion
	}
	return s.perform(ctx, r, "verify", func() error {
		runs, err := s.boundRuns(ctx, r)
		if err != nil {
			return err
		}
		if err := s.Driver.Verify(ctx, destination, version, runs); err != nil {
			if !r.RollbackAt.IsZero() {
				return err
			}
			s.event(r, "verify.failed", true, 0)
			return s.record(ctx, func() error { return s.Store.RequestUpgradeRollback(ctx, r.ID, "verification_failed") })
		}
		return s.record(ctx, func() error { return s.Store.RecordUpgradeVerified(ctx, r.ID) })
	})
}

func (s Service) finish(ctx context.Context, r Receipt, source core.Sandbox) error {
	replacement := r.DestinationResourceID != "" && r.DestinationResourceID != r.SourceResourceID
	if replacement {
		deleted, err := s.resourceDeleted(ctx, r.SessionID, r.SourceResourceID)
		if err != nil {
			return err
		}
		if !deleted {
			return s.deleteResource(ctx, r, source)
		}
	} else if r.CheckpointDeletedAt.IsZero() {
		return s.deleteCheckpoint(ctx, r, source)
	}
	return s.perform(ctx, r, "resume", func() error {
		return s.record(ctx, func() error { return s.Store.FinishSandboxUpgrade(ctx, s.Queue, r) })
	})
}

func (s Service) boundRuns(ctx context.Context, r Receipt) ([]core.AgentRun, error) {
	deliveries, err := s.Store.Deliveries(ctx, r.SessionID)
	if err != nil {
		return nil, err
	}
	var runs []core.AgentRun
	for _, delivery := range deliveries {
		if delivery.AgentRun.SandboxID == r.SandboxID && delivery.AgentRun.ThreadID != "" {
			runs = append(runs, delivery.AgentRun)
		}
	}
	return runs, nil
}

func (s Service) resourceDeleted(ctx context.Context, sessionID, resourceID string) (bool, error) {
	resources, err := s.Store.SandboxResources(ctx, sessionID)
	if err != nil {
		return false, err
	}
	for _, resource := range resources {
		if resource.ID == resourceID {
			return !resource.DeletedAt.IsZero(), nil
		}
	}
	return false, fmt.Errorf("upgrade resource not retained")
}

func (s Service) deleteResource(ctx context.Context, r Receipt, owned core.Sandbox) error {
	return s.perform(ctx, r, "resource-cleanup", func() error {
		if err := s.Driver.DeleteResource(ctx, owned); err != nil {
			return err
		}
		return s.record(ctx, func() error { return s.Store.RecordSandboxResourceDeleted(ctx, owned) })
	})
}
func (s Service) deleteCheckpoint(ctx context.Context, r Receipt, source core.Sandbox) error {
	return s.perform(ctx, r, "checkpoint-cleanup", func() error {
		if err := s.Driver.DeleteCheckpoint(ctx, source, r.Checkpoint); err != nil {
			return err
		}
		return s.record(ctx, func() error { return s.Store.RecordUpgradeCheckpointDeleted(ctx, r.ID) })
	})
}
func (s Service) requireClaim(ctx context.Context) error {
	if s.Claim == nil {
		return fmt.Errorf("upgrade executor claim is not configured")
	}
	return s.Claim(ctx)
}
func (s Service) record(ctx context.Context, fn func() error) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return fn()
}
func (s Service) perform(ctx context.Context, r Receipt, step string, fn func() error) error {
	start := time.Now()
	s.event(r, step+".started", false, 0)
	err := fn()
	if err != nil {
		s.event(r, step+".failed", true, time.Since(start))
	}
	s.event(r, step+".finished", err != nil, time.Since(start))
	source := "upgrade:" + r.ID
	if err != nil {
		if claimErr := s.requireClaim(ctx); claimErr != nil {
			return claimErr
		}
		// Do not put native output, route credentials, or unbounded provider errors
		// into diagnostics. The operation and exact custody provide correlation.
		detail := "workspace upgrade " + step + " failed"
		if saveErr := s.Store.SetExecutionAttention(ctx, r.SessionID, source, detail); saveErr != nil {
			return saveErr
		}
		return fmt.Errorf("%s; delivery remains held", detail)
	}
	return s.Store.ClearExecutionAttention(ctx, r.SessionID, source)
}
func (s Service) event(r Receipt, name string, failed bool, duration time.Duration) {
	if s.Emit == nil {
		return
	}
	outcome := ""
	if name == "resume.finished" && !failed {
		outcome = "upgraded"
		if !r.RollbackAt.IsZero() {
			outcome = "rolled_back"
		}
	}
	s.Emit(telemetry.Event{Name: "dorf.upgrade." + name, At: time.Now(), Failed: failed, Attributes: map[string]any{
		"dorf.upgrade_id": r.ID, "dorf.provider": s.Provider, "dorf.requested_at": r.RequestedAt.Format(time.RFC3339Nano), "dorf.session_id": r.SessionID, "dorf.sandbox_id": r.SandboxID,
		"dorf.source_resource_id": r.SourceResourceID, "dorf.source_provider_id": r.SourceProviderID, "dorf.destination_provider_id": r.DestinationProviderID, "dorf.destination_resource_id": r.DestinationResourceID,
		"dorf.checkpoint_reference": r.Checkpoint.Reference, "dorf.upgrade_outcome": outcome, "dorf.failure_code": r.FailureCode, "dorf.package_version": r.Version,
		"dorf.previous_package_version": r.PreviousVersion, "duration_ms": duration.Milliseconds(),
	}})
}

func (s Service) prepare(ctx context.Context, r Receipt, source core.Sandbox) error {
	return s.perform(ctx, r, "prepare", func() error {
		version, err := s.Driver.InspectPackage(ctx, source, r.Request)
		if err != nil {
			return err
		}
		if version == "" {
			return fmt.Errorf("package inspection omitted previous version")
		}
		return s.record(ctx, func() error { return s.Store.RecordUpgradePreparation(ctx, r.ID, version) })
	})
}

// ReportAccepted logs the retained request receipt. Replays may emit the same
// upgrade identity again; they never create a second operation or hold.
func ReportAccepted(receipt Receipt, emit func(telemetry.Event)) {
	service := Service{Emit: emit}
	service.event(receipt, "accepted", false, 0)
	service.event(receipt, "delivery-held", false, 0)
}
