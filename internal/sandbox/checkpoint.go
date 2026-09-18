package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
)

// Checkpoint identifies one immutable recovery point. SourceID is an opaque
// provider locator; the adapter owns its interpretation and attestation.
type Checkpoint struct {
	Key       string `json:"key"`
	Reference string `json:"reference"`
	SourceID  string `json:"source_id"`
}

var checkpointKey = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var checkpointNonce = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidateCheckpointKey(key string) error {
	if !checkpointKey.MatchString(key) {
		return fmt.Errorf("checkpoint requires a stable lowercase operation key")
	}
	return nil
}

// OwnedCheckpointName lets providers without snapshot metadata bind a stable
// snapshot name to the exact source owner, even after the source VM is deleted.
func OwnedCheckpointName(owner Ownership, key string) (string, error) {
	if err := ValidateCheckpointKey(key); err != nil {
		return "", err
	}
	if owner.SessionID == "" || owner.SandboxID == "" || !checkpointNonce.MatchString(owner.OwnershipNonce) {
		return "", OwnershipErrorf("checkpoint requires complete resource ownership")
	}
	encoded, err := json.Marshal([4]string{owner.SessionID, owner.SandboxID, owner.OwnershipNonce, key})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("dorf-cp-%x", sha256.Sum256(encoded))[:63], nil
}

// Checkpointer is optional. Callers must durably hold new delivery and settle
// native mutations before capture or restore. Capture may leave a VM stopped
// after failure; the hold must survive until recovery has been verified.
type Checkpointer interface {
	CaptureCheckpoint(context.Context, Ownership, string) (Checkpoint, error)
	RestoreCheckpoint(context.Context, Ownership, Ownership, Checkpoint) (string, error)
	DeleteCheckpoint(context.Context, Ownership, Checkpoint) error
}
