package hostsetup

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/aphronio/dorf/internal/deployment"
)

const (
	// DatabaseImage is the reviewed recovery reference for Dorf's supported
	// Linux/amd64 PostgreSQL image. DatabaseImageID is the exact local image
	// identity that reference resolved to when this deployment contract was
	// accepted; Compose refuses a moved tag instead of upgrading implicitly.
	DatabaseImage   = "postgres:17.10-bookworm"
	DatabaseImageID = "sha256:8164afa59e26be9f78959e538d6c9da8553d67e601d8ebd0d5e9cbf558c3986e"
	DatabasePort    = 54329
)

// InitializeDatabase persists the complete PostgreSQL identity before the
// Compose lifecycle creates or attaches any Docker resource. It performs no
// Docker, package-manager, service-manager, or privileged operation.
func InitializeDatabase(path string) (deployment.Database, error) {
	return initializeDatabase(path, rand.Reader)
}

func initializeDatabase(path string, random io.Reader) (deployment.Database, error) {
	stored, found, err := deployment.Load(path)
	if err != nil {
		return deployment.Database{}, err
	}
	if found {
		if stored.ControlReaderKey == "" {
			key, err := generateControlReaderKey(random)
			if err != nil {
				return deployment.Database{}, err
			}
			if err := deployment.EnsureControlReaderKey(path, key); err != nil {
				return deployment.Database{}, err
			}
		} else if err := deployment.ValidateControlReaderKey(stored.ControlReaderKey); err != nil {
			return deployment.Database{}, err
		}
		return stored.Database, nil
	}

	passwordBytes := make([]byte, 32)
	if _, err := io.ReadFull(random, passwordBytes); err != nil {
		return deployment.Database{}, fmt.Errorf("generate PostgreSQL credential: %w", err)
	}
	database := deployment.Database{
		Host:        "127.0.0.1",
		Port:        DatabasePort,
		Name:        "dorf",
		User:        "dorf",
		Password:    base64.RawURLEncoding.EncodeToString(passwordBytes),
		Image:       DatabaseImage,
		ImageID:     DatabaseImageID,
		VolumeState: deployment.DatabaseVolumePending,
	}
	controlReaderKey, err := generateControlReaderKey(random)
	if err != nil {
		return deployment.Database{}, err
	}
	if err := deployment.Save(path, deployment.Config{Database: database, ControlReaderKey: controlReaderKey}); err != nil {
		return deployment.Database{}, err
	}
	return database, nil
}

func generateControlReaderKey(random io.Reader) (string, error) {
	contents := make([]byte, 32)
	if _, err := io.ReadFull(random, contents); err != nil {
		return "", fmt.Errorf("generate control reader credential: %w", err)
	}
	return hex.EncodeToString(contents), nil
}
