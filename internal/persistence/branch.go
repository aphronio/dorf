package persistence

import (
	"fmt"
	"strings"
	"time"
)

// BranchRequest selects one published source checkpoint and a stable identity
// for a new, independent Session. The caller must own this operator action.
type BranchRequest struct {
	ID              string `json:"id"`
	SourceSessionID string `json:"source_session_id"`
	Repository      string `json:"repository"`
	SnapshotID      string `json:"snapshot_id"`
}

func (r BranchRequest) Validate() error {
	for _, value := range []string{r.ID, r.SourceSessionID, r.Repository} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 200 {
			return fmt.Errorf("branch requires exact bounded identities")
		}
	}
	if !snapshotPattern.MatchString(r.SnapshotID) {
		return fmt.Errorf("branch requires an exact snapshot ID")
	}
	return nil
}

// BranchReceipt is the durable gate and lineage for a destination Session.
// Restored means files and package are installed, so client file preparation
// may begin. Ready means fresh route authority and native resume were verified.
type BranchReceipt struct {
	BranchRequest
	DestinationSessionID string     `json:"destination_session_id"`
	DestinationSandboxID string     `json:"destination_sandbox_id"`
	Checkpoint           Checkpoint `json:"checkpoint"`
	ThreadID             string     `json:"thread_id"`
	RequestedAt          time.Time  `json:"requested_at"`
	RestoredAt           time.Time  `json:"restored_at,omitzero"`
	ReleaseRequestedAt   time.Time  `json:"release_requested_at,omitzero"`
	ReadyAt              time.Time  `json:"ready_at,omitzero"`
}
