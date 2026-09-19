package persistence

import "time"

// Workspace describes configured workspace coverage, not proof that current
// files have reached backup storage. It contains no repository credentials or
// native runtime paths. A nil checkpoint means none has been published.
type Workspace struct {
	ObservedAt                 time.Time  `json:"observed_at"`
	Path                       string     `json:"path"`
	BackupEnabled              bool       `json:"backup_enabled"`
	IdleDelaySeconds           *int       `json:"idle_delay_seconds"`
	LastSuccessfulCheckpointAt *time.Time `json:"last_successful_checkpoint_at"`
}
