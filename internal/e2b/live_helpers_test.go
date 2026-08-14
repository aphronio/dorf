package e2b

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func liveOwnership(t *testing.T, purpose string) Ownership {
	t.Helper()
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		t.Fatal(err)
	}
	nonce := hex.EncodeToString(nonceBytes)
	return Ownership{
		JobID:          "e2b-" + purpose + "-proof-" + nonce[:12],
		SandboxID:      "dorf-e2b-" + purpose + "-" + nonce[:12],
		OwnershipNonce: nonce,
	}
}
