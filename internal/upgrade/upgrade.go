// Package upgrade coordinates package changes under an existing Job's delivery
// hold. Receipts describe effects; the current operation is derived from them.
package upgrade

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/aphronio/dorf/internal/core"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

type Request struct {
	ID          string `json:"id"`
	JobID       string `json:"job_id"`
	SandboxID   string `json:"sandbox_id"`
	PackagePath string `json:"package_path"`
	Version     string `json:"version"`
}

var packagePath = regexp.MustCompile(`^/nix/store/[a-z0-9]{32}-[a-zA-Z0-9+._?-]+$`)
var packageVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func (r Request) Validate() error {
	if err := provider.ValidateCheckpointKey(r.ID); err != nil {
		return err
	}
	if r.JobID == "" || r.SandboxID == "" || !packagePath.MatchString(r.PackagePath) || !packageVersion.MatchString(r.Version) {
		return fmt.Errorf("upgrade requires exact Job, Sandbox, staged Nix path, and package version")
	}
	return nil
}

// Receipt contains only recovery facts. Resource ownership tokens are carried
// separately by Core's private Sandbox value and are never projected or logged.
type Receipt struct {
	Request
	SourceResourceID      string              `json:"source_resource_id"`
	SourceProviderID      string              `json:"source_provider_id,omitempty"`
	DestinationProviderID string              `json:"destination_provider_id,omitempty"`
	DestinationResourceID string              `json:"destination_resource_id,omitempty"`
	RequestedAt           time.Time           `json:"requested_at"`
	PreviousVersion       string              `json:"previous_version,omitempty"`
	QuiescedAt            time.Time           `json:"quiesced_at,omitempty"`
	Checkpoint            provider.Checkpoint `json:"checkpoint"`
	ActivatedAt           time.Time           `json:"activated_at,omitempty"`
	RollbackAt            time.Time           `json:"rollback_at,omitempty"`
	FailureCode           string              `json:"failure_code,omitempty"`
	RestoredAt            time.Time           `json:"restored_at,omitempty"`
	VerifiedAt            time.Time           `json:"verified_at,omitempty"`
	CheckpointDeletedAt   time.Time           `json:"checkpoint_deleted_at,omitempty"`
	FinishedAt            time.Time           `json:"finished_at,omitempty"`
}

func (r Receipt) Outcome() string {
	if r.FinishedAt.IsZero() {
		return ""
	}
	if !r.RollbackAt.IsZero() {
		return "rolled_back"
	}
	return "upgraded"
}

// Driver operations reconcile exact ownership before mutation and are safe to
// repeat after a lost receipt while delivery remains held. Verify must prove
// the bound native conversation without starting a new agent Turn.
type Driver interface {
	InspectPackage(context.Context, core.Sandbox, Request) (string, error)
	Quiesce(context.Context, core.Sandbox, []core.AgentRun) error
	Capture(context.Context, core.Sandbox, string) (provider.Checkpoint, error)
	Activate(context.Context, core.Sandbox, Request) error
	ReplacesResource() bool
	Restore(context.Context, core.Sandbox, core.Sandbox, provider.Checkpoint) (string, error)
	Verify(context.Context, core.Sandbox, string, []core.AgentRun) error
	DeleteResource(context.Context, core.Sandbox) error
	DeleteCheckpoint(context.Context, core.Sandbox, provider.Checkpoint) error
}

// Status is a projection; no separate upgrade phase is persisted.
func (r Receipt) Status() string {
	switch {
	case !r.FinishedAt.IsZero():
		return "ready"
	case !r.RollbackAt.IsZero() && r.RestoredAt.IsZero():
		return "rolling_back"
	case !r.ActivatedAt.IsZero() || !r.RestoredAt.IsZero():
		return "verifying"
	default:
		return "upgrading"
	}
}
