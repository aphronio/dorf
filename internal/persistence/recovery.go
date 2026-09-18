package persistence

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// RecoveryRequest selects one exact published checkpoint. ID owns the
// delivery hold and makes an operator retry idempotent.
type RecoveryRequest struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	SandboxID  string `json:"sandbox_id"`
	Repository string `json:"repository"`
	SnapshotID string `json:"snapshot_id"`
}

func (r RecoveryRequest) Validate() error {
	for _, value := range []string{r.ID, r.SessionID, r.SandboxID, r.Repository} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 256 {
			return fmt.Errorf("recovery requires exact bounded identities")
		}
	}
	if len(r.ID) > 200 {
		return fmt.Errorf("recovery identity must be at most 200 characters")
	}
	if !snapshotPattern.MatchString(r.SnapshotID) {
		return fmt.Errorf("recovery requires an exact snapshot ID")
	}
	return nil
}

// EffectivePackage is empty for the profile's base package. A non-empty value
// is the exact successfully activated Nix package generation captured in the
// checkpoint.
type EffectivePackage struct {
	UpgradeID   string `json:"upgrade_id,omitempty"`
	PackagePath string `json:"package_path,omitempty"`
	Version     string `json:"version,omitempty"`
}

// RecoveryReceipt contains only the facts needed to reconcile a replacement.
// Provider ownership tokens remain in Core's private Sandbox values.
type RecoveryReceipt struct {
	RecoveryRequest
	Checkpoint            Checkpoint       `json:"checkpoint"`
	SourceResourceID      string           `json:"source_resource_id"`
	SourceProviderID      string           `json:"source_provider_id,omitempty"`
	DestinationResourceID string           `json:"destination_resource_id,omitempty"`
	DestinationProviderID string           `json:"destination_provider_id,omitempty"`
	Package               EffectivePackage `json:"package,omitempty"`
	RequestedAt           time.Time        `json:"requested_at"`
	DestinationDeletedAt  time.Time        `json:"destination_deleted_at,omitempty"`
	VerifiedAt            time.Time        `json:"verified_at,omitempty"`
	SourceDeletedAt       time.Time        `json:"source_deleted_at,omitempty"`
	FinishedAt            time.Time        `json:"finished_at,omitempty"`
}

// RecoveryDriver owns repeatable provider effects. Restore reconciles creation
// of the exact destination ownership before restoring the exact snapshot.
// VerifyAndRenew verifies the captured native prefix and effective package,
// then installs fresh route authority in the replacement without starting a
// new Turn. Repeating either method after a lost receipt must be safe.
type RecoveryDriver interface {
	Restore(context.Context, core.Session, core.Sandbox, Checkpoint) (string, error)
	VerifyAndRenew(context.Context, core.Session, core.Sandbox, Checkpoint, EffectivePackage) error
	DeleteResource(context.Context, core.Sandbox) error
}

type RecoveryStore interface {
	WithSessionFence(context.Context, string, func() error) error
	Session(context.Context, string) (core.Session, error)
	Sandbox(context.Context, string) (core.Sandbox, error)
	SessionRecoveries(context.Context, string) ([]RecoveryReceipt, error)
	SandboxResource(context.Context, string, string, string) (core.Sandbox, error)
	RecoveryNativeStateSafe(context.Context, RecoveryReceipt) (bool, error)
	RecordRecoveryRestored(context.Context, RecoveryReceipt, string) error
	RecordRecoveryVerified(context.Context, string) error
	RecordSandboxResourceDeleted(context.Context, core.Sandbox) error
	AbandonCheckpointRecoveryForCleanup(context.Context, RecoveryReceipt) error
	FinishCheckpointRecovery(context.Context, string, RecoveryReceipt) error
	SetExecutionAttention(context.Context, string, string, string) error
	ClearExecutionAttention(context.Context, string, string) error
}

type RecoveryService struct {
	Store  RecoveryStore
	Driver RecoveryDriver
	Queue  string
	Claim  func(context.Context) error
}

// Reconcile performs at most one repeatable recovery effect under the same Session
// fence as native delivery. Pending queued input remains durable behind the
// exact recovery hold.
func (s RecoveryService) Reconcile(ctx context.Context, sessionID string) (bool, error) {
	var progressed bool
	err := s.Store.WithSessionFence(ctx, sessionID, func() error {
		session, err := s.Store.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if task, ok := absurd.TaskFromContext(ctx); ok && task.TaskID() != session.CurrentTaskID {
			return fmt.Errorf("recovery executor no longer owns the Session task")
		}
		if !session.AdmissionOpen || session.CleanupState != core.CleanupPending {
			return nil
		}
		receipts, err := s.Store.SessionRecoveries(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, receipt := range receipts {
			if !receipt.FinishedAt.IsZero() {
				continue
			}
			if err := s.requireSafe(ctx, receipt); err != nil {
				return s.attention(ctx, receipt, err)
			}
			if err := s.requireClaim(ctx); err != nil {
				return err
			}
			err = s.step(ctx, session, receipt)
			if err != nil {
				return s.attention(ctx, receipt, err)
			}
			progressed = true
			return s.Store.ClearExecutionAttention(ctx, sessionID, "recovery:"+receipt.ID)
		}
		return nil
	})
	return progressed, err
}

func (s RecoveryService) step(ctx context.Context, session core.Session, receipt RecoveryReceipt) error {
	if receipt.DestinationProviderID == "" || receipt.VerifiedAt.IsZero() {
		destination, err := s.Store.SandboxResource(ctx, session.ID, receipt.SandboxID, receipt.DestinationResourceID)
		if err != nil {
			return err
		}
		if receipt.DestinationProviderID == "" {
			providerID, err := s.Driver.Restore(ctx, session, destination, receipt.Checkpoint)
			if err != nil {
				return err
			}
			return s.record(ctx, func() error { return s.Store.RecordRecoveryRestored(ctx, receipt, providerID) })
		}
		if err := s.Driver.VerifyAndRenew(ctx, session, destination, receipt.Checkpoint, receipt.Package); err != nil {
			return err
		}
		return s.record(ctx, func() error { return s.Store.RecordRecoveryVerified(ctx, receipt.ID) })
	}
	if receipt.SourceDeletedAt.IsZero() {
		source, err := s.Store.SandboxResource(ctx, session.ID, receipt.SandboxID, receipt.SourceResourceID)
		if err != nil {
			return err
		}
		if err := s.Driver.DeleteResource(ctx, source); err != nil {
			return err
		}
		return s.record(ctx, func() error { return s.Store.RecordSandboxResourceDeleted(ctx, source) })
	}
	return s.Store.FinishCheckpointRecovery(ctx, s.Queue, receipt)
}

func (s RecoveryService) requireSafe(ctx context.Context, receipt RecoveryReceipt) error {
	safe, err := s.Store.RecoveryNativeStateSafe(ctx, receipt)
	if err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("native work exists beyond the selected checkpoint")
	}
	return nil
}

func (s RecoveryService) requireClaim(ctx context.Context) error {
	if s.Claim == nil {
		return fmt.Errorf("recovery executor claim is not configured")
	}
	return s.Claim(ctx)
}

func (s RecoveryService) record(ctx context.Context, fn func() error) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	return fn()
}

func (s RecoveryService) attention(ctx context.Context, receipt RecoveryReceipt, cause error) error {
	if err := s.requireClaim(ctx); err != nil {
		return err
	}
	detail := "checkpoint recovery requires operator attention; delivery remains held"
	if err := s.Store.SetExecutionAttention(ctx, receipt.SessionID, "recovery:"+receipt.ID, detail); err != nil {
		return err
	}
	return fmt.Errorf("%s: %w", detail, cause)
}

// PrepareCleanup removes a replacement reserved by an unfinished recovery.
// The ordinary cleanup path owns the active resource and releases remaining
// delivery holds only after every owned resource is gone.
func (s RecoveryService) PrepareCleanup(ctx context.Context, sessionID string) error {
	return s.Store.WithSessionFence(ctx, sessionID, func() error {
		session, err := s.Store.Session(ctx, sessionID)
		if err != nil {
			return err
		}
		if task, ok := absurd.TaskFromContext(ctx); ok && task.TaskID() != session.CurrentTaskID {
			return fmt.Errorf("recovery cleanup no longer owns the Session task")
		}
		if session.AdmissionOpen || session.CleanupState != core.CleanupScheduled {
			return fmt.Errorf("recovery cleanup requires the scheduled Session cleanup owner")
		}
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		receipts, err := s.Store.SessionRecoveries(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, receipt := range receipts {
			if !receipt.FinishedAt.IsZero() {
				continue
			}
			if err := s.prepareReceiptCleanup(ctx, sessionID, receipt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s RecoveryService) prepareReceiptCleanup(ctx context.Context, sessionID string, receipt RecoveryReceipt) error {
	active, err := s.Store.Sandbox(ctx, receipt.SandboxID)
	if err != nil {
		return err
	}
	if active.ResourceID != receipt.SourceResourceID {
		return fmt.Errorf("cleanup cannot abandon adopted recovery custody")
	}
	if receipt.DestinationDeletedAt.IsZero() {
		destination, err := s.Store.SandboxResource(ctx, sessionID, receipt.SandboxID, receipt.DestinationResourceID)
		if err != nil {
			return err
		}
		if err := s.requireClaim(ctx); err != nil {
			return err
		}
		if err := s.Driver.DeleteResource(ctx, destination); err != nil {
			return err
		}
		if err := s.record(ctx, func() error { return s.Store.RecordSandboxResourceDeleted(ctx, destination) }); err != nil {
			return err
		}
	}
	return s.record(ctx, func() error { return s.Store.AbandonCheckpointRecoveryForCleanup(ctx, receipt) })
}
