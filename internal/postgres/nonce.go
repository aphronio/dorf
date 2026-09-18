package postgres

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func ownershipNonce() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate ownership nonce: %w", err)
	}
	return hex.EncodeToString(value), nil
}
