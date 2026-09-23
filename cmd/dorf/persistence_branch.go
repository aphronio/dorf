package main

import (
	"context"
	"fmt"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/persistence"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

// ReconcileBranch runs before the ordinary direct route/bootstrap path. A
// restored branch remains held until the client has replaced application
// authority and explicitly requests release. The source is never mutated.
func (e checkpointExecution) ReconcileBranch(ctx context.Context, sessionID string) (bool, error) {
	receipt, exists, err := e.resolver.store.SessionCheckpointBranch(ctx, sessionID)
	if err != nil || !exists {
		return false, err
	}
	if !receipt.ReadyAt.IsZero() {
		return false, nil
	}
	if !receipt.RestoredAt.IsZero() && receipt.ReleaseRequestedAt.IsZero() {
		return true, nil
	}
	return absurdruntime.WithHeartbeat(ctx, func(workCtx context.Context) (bool, error) {
		return e.reconcileBranch(workCtx, receipt)
	})
}

func (e checkpointExecution) reconcileBranch(ctx context.Context, receipt persistence.BranchReceipt) (bool, error) {
	if receipt.RestoredAt.IsZero() {
		return true, e.restoreBranch(ctx, receipt)
	}
	return false, e.releaseBranch(ctx, receipt)
}

func (e checkpointExecution) restoreBranch(ctx context.Context, receipt persistence.BranchReceipt) error {
	return e.resolver.store.WithSessionFence(ctx, receipt.DestinationSessionID, func() error {
		current, err := e.branchAuthority(ctx, receipt)
		if err != nil || !current.RestoredAt.IsZero() {
			return err
		}
		if err := e.restoreBranchSnapshot(ctx, current); err != nil {
			return err
		}
		if err := absurdruntime.RequireClaim(ctx); err != nil {
			return err
		}
		return e.resolver.store.RecordCheckpointBranchRestored(ctx, current.ID)
	})
}

func (e checkpointExecution) restoreBranchSnapshot(ctx context.Context, receipt persistence.BranchReceipt) error {
	driver, err := e.resolver.checkpointDriver(ctx, core.SandboxProfileRef{Name: receipt.Checkpoint.ProfileName, Revision: receipt.Checkpoint.ProfileRevision})
	if err != nil {
		return err
	}
	source, err := e.resolver.store.Sandbox(ctx, receipt.Checkpoint.SandboxID)
	if err != nil {
		return err
	}
	destination, err := e.resolver.store.Sandbox(ctx, receipt.DestinationSandboxID)
	if err != nil {
		return err
	}
	if source.SessionID != receipt.SourceSessionID || destination.SessionID != receipt.DestinationSessionID || destination.ProviderID == "" {
		return fmt.Errorf("branch restore has no exact source and destination custody")
	}
	if _, err := driver.restoreInto(ctx, source, destination, receipt.Checkpoint); err != nil {
		return err
	}
	return nil
}

func (e checkpointExecution) releaseBranch(ctx context.Context, receipt persistence.BranchReceipt) error {
	// The new route is installed only after the client's release request. The
	// hold still denies native access until resume and continuity verification.
	if err := e.Execution.ExecuteSandboxAction(ctx, receipt.DestinationSessionID, receipt.DestinationSandboxID, core.ActionRouteCreate); err != nil {
		return err
	}
	return e.resolver.store.WithSessionFence(ctx, receipt.DestinationSessionID, func() error {
		current, err := e.branchAuthority(ctx, receipt)
		if err != nil || !current.ReadyAt.IsZero() {
			return err
		}
		if current.ReleaseRequestedAt.IsZero() {
			return fmt.Errorf("branch release is not requested")
		}
		if err := e.verifyBranchThread(ctx, current); err != nil {
			return err
		}
		if err := absurdruntime.RequireClaim(ctx); err != nil {
			return err
		}
		return e.resolver.store.FinishCheckpointBranch(ctx, current)
	})
}

func (e checkpointExecution) verifyBranchThread(ctx context.Context, receipt persistence.BranchReceipt) error {
	driver, err := e.resolver.checkpointDriver(ctx, core.SandboxProfileRef{Name: receipt.Checkpoint.ProfileName, Revision: receipt.Checkpoint.ProfileRevision})
	if err != nil {
		return err
	}
	destination, err := e.resolver.store.Sandbox(ctx, receipt.DestinationSandboxID)
	if err != nil {
		return err
	}
	return driver.capture.agent.VerifyUpgrade(ctx, checkpointOwner(destination), receipt.ThreadID)
}

func (e checkpointExecution) branchAuthority(ctx context.Context, expected persistence.BranchReceipt) (persistence.BranchReceipt, error) {
	session, err := e.resolver.store.Session(ctx, expected.DestinationSessionID)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	if task, ok := absurd.TaskFromContext(ctx); !ok || task.TaskID() != session.CurrentTaskID {
		return persistence.BranchReceipt{}, fmt.Errorf("branch executor no longer owns the Session task")
	}
	if !session.AdmissionOpen || session.CleanupState != core.CleanupPending || session.ThreadID != expected.ThreadID {
		return persistence.BranchReceipt{}, fmt.Errorf("branch Session is not open with its exact native binding")
	}
	current, exists, err := e.resolver.store.SessionCheckpointBranch(ctx, session.ID)
	if err != nil {
		return persistence.BranchReceipt{}, err
	}
	if !exists || current.BranchRequest != expected.BranchRequest || current.DestinationSandboxID != expected.DestinationSandboxID {
		return persistence.BranchReceipt{}, fmt.Errorf("branch custody changed")
	}
	if err := e.requireBranchHold(ctx, current); err != nil {
		return persistence.BranchReceipt{}, err
	}
	if err := absurdruntime.RequireClaim(ctx); err != nil {
		return persistence.BranchReceipt{}, err
	}
	return current, nil
}

func (e checkpointExecution) requireBranchHold(ctx context.Context, receipt persistence.BranchReceipt) error {
	holds, err := e.resolver.store.SessionDeliveryHolds(ctx, receipt.DestinationSessionID)
	if err != nil {
		return err
	}
	for _, hold := range holds {
		if hold.ID == receipt.ID && hold.SandboxID == receipt.DestinationSandboxID && hold.Reason == "checkpoint_branch" {
			return nil
		}
	}
	return fmt.Errorf("branch lost its exact destination hold")
}
