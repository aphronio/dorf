// Package persistence owns the provider-independent facts needed to capture
// and restore one Sandbox without making a provider VM durable.
package persistence

import (
	"errors"
	"time"
)

var (
	ErrCheckpointIneligible = errors.New("Sandbox is not eligible for a checkpoint")
	ErrCheckpointSuperseded = errors.New("checkpoint boundary was superseded")
	ErrCheckpointNotFound   = errors.New("checkpoint not found")
)

// CaptureBoundary is a comparable observation of the durable facts that can
// affect a Sandbox while an unfenced upload runs. NativeRevision is the exact
// native dispatch cutoff. Eligibility also requires settled native work and
// no unresolved mutation.
//
// Cleanup selects the closed-admission eligibility rule. It does not create a
// different checkpoint kind or storage path. Eligible is derived from current
// facts and must still be true when publication rechecks the boundary.
type CaptureBoundary struct {
	SessionID          string    `json:"session_id"`
	SandboxID          string    `json:"sandbox_id"`
	ResourceID         string    `json:"resource_id"`
	ProfileName        string    `json:"profile_name"`
	ProfileRevision    string    `json:"profile_revision"`
	EffectiveUpgradeID string    `json:"effective_upgrade_id,omitempty"`
	LastActivityAt     time.Time `json:"last_activity_at"`
	NativeRevision     int64     `json:"native_revision"`
	DeliveryHoldCount  int64     `json:"delivery_hold_count"`
	Cleanup            bool      `json:"cleanup"`
	Eligible           bool      `json:"eligible"`
}

// Reference identifies an immutable restic snapshot within one configured
// repository. Repository is a logical configuration identity, never a URL or
// credential.
type Reference struct {
	Repository string `json:"repository"`
	SnapshotID string `json:"snapshot_id"`
}

// Checkpoint is one successfully published recovery fact. Failed and cancelled
// attempts remain diagnostic events and never enter this history.
type Checkpoint struct {
	CaptureBoundary
	Reference
	PublishedAt time.Time `json:"published_at"`
}
