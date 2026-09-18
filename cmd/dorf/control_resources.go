package main

import (
	"context"
	"time"

	"github.com/aphronio/dorf/internal/controlapi"
	"github.com/aphronio/dorf/internal/core"
)

func (a controlAPISessions) projectSessionResources(ctx context.Context, session core.Session, executionState string, attention *controlapi.Attention, task taskResultView, owned []core.Sandbox) (controlapi.Session, error) {
	view, err := publicSession(session, executionState, attention, task, owned)
	if err != nil {
		return controlapi.Session{}, err
	}
	resources, err := a.store.SandboxResources(ctx, session.ID)
	if err != nil {
		return controlapi.Session{}, err
	}
	bySandbox := make(map[string][]controlapi.SandboxResource)
	for _, resource := range resources {
		bySandbox[resource.SandboxID] = append(bySandbox[resource.SandboxID], controlapi.SandboxResource{
			ID: resource.ID, ProviderID: resource.ProviderID, ReservedAt: resource.ReservedAt,
			ObservedAt: optionalResourceTime(resource.ObservedAt), DeletedAt: optionalResourceTime(resource.DeletedAt),
		})
	}
	for i := range view.Sandboxes {
		view.Sandboxes[i].Resources = bySandbox[view.Sandboxes[i].ID]
	}
	holds, err := a.store.SessionDeliveryHolds(ctx, session.ID)
	if err != nil {
		return controlapi.Session{}, err
	}
	for i := range view.Sandboxes {
		for _, hold := range holds {
			if hold.SandboxID == view.Sandboxes[i].ID {
				view.Sandboxes[i].DeliveryHold = &controlapi.SandboxDeliveryHold{ID: hold.ID, Reason: hold.Reason, RequestedAt: hold.RequestedAt}
			}
		}
	}
	if err := a.projectUpgrades(ctx, session, &view); err != nil {
		return controlapi.Session{}, err
	}
	return view, nil
}

func optionalResourceTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	return &value
}

func (a controlAPISessions) messageWaitReason(ctx context.Context, delivery core.Delivery) (string, error) {
	if delivery.AgentRun.State != core.AgentRunPending || delivery.Message.Intent != core.MessageFollow {
		return "", nil
	}
	holds, err := a.store.SessionDeliveryHolds(ctx, delivery.Message.SessionID)
	if err != nil {
		return "", err
	}
	for _, hold := range holds {
		if hold.SandboxID == delivery.AgentRun.SandboxID {
			return hold.Reason, nil
		}
	}
	return "", nil
}

func (a controlAPISessions) projectUpgrades(ctx context.Context, session core.Session, view *controlapi.Session) error {
	receipts, err := a.store.SessionUpgrades(ctx, session.ID)
	if err != nil {
		return err
	}
	for i := range view.Sandboxes {
		for _, receipt := range receipts {
			if receipt.SandboxID != view.Sandboxes[i].ID {
				continue
			}
			status := receipt.Status()
			if receipt.FinishedAt.IsZero() && session.WorkflowAttentionSource == "upgrade:"+receipt.ID && session.WorkflowAttention != "" {
				status = "failed"
			}
			view.Sandboxes[i].Upgrades = append(view.Sandboxes[i].Upgrades, controlapi.SandboxUpgrade{
				ID: receipt.ID, Status: status, SourceResourceID: receipt.SourceResourceID, DestinationResourceID: receipt.DestinationResourceID,
				PackageVersion: receipt.Version, PreviousVersion: receipt.PreviousVersion, RequestedAt: receipt.RequestedAt,
				VerifiedAt: optionalResourceTime(receipt.VerifiedAt), FinishedAt: optionalResourceTime(receipt.FinishedAt),
				Outcome: receipt.Outcome(), FailureCode: receipt.FailureCode, CheckpointReference: receipt.Checkpoint.Reference,
			})
		}
	}
	return nil
}
