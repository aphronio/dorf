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
	DatabasePort = 54329
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
		if err := deployment.ValidateControlReaderKey(stored.ControlReaderKey); err != nil {
			return deployment.Database{}, err
		}
		return stored.Database, nil
	}

	passwordBytes := make([]byte, 32)
	if _, err := io.ReadFull(random, passwordBytes); err != nil {
		return deployment.Database{}, fmt.Errorf("generate PostgreSQL credential: %w", err)
	}
	database := deployment.Database{
		Host:     "127.0.0.1",
		Port:     DatabasePort,
		Name:     "dorf",
		User:     "dorf",
		Password: base64.RawURLEncoding.EncodeToString(passwordBytes),
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
