package hostsetup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/deployment"
)

func TestInitializeDatabasePersistsPendingIdentityWithoutDocker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "deployment.json")
	database, err := initializeDatabase(path, bytes.NewReader(bytes.Repeat([]byte{0x5a}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	if database.Host != "127.0.0.1" || database.Port != DatabasePort || database.Name != "dorf" || database.User != "dorf" {
		t.Fatalf("unexpected PostgreSQL identity: %#v", database)
	}
	if database.Image != DatabaseImage || database.ImageID != DatabaseImageID || database.VolumeState != deployment.DatabaseVolumePending {
		t.Fatalf("unexpected PostgreSQL image/volume authority: %#v", database)
	}
	if database.Password == "" {
		t.Fatal("PostgreSQL credential was not generated")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("deployment mode=%04o want=0600", info.Mode().Perm())
	}
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.Database != database || stored.ControlReaderKey != strings.Repeat("5a", 32) {
		t.Fatalf("stored=%#v found=%t error=%v", stored, found, err)
	}
}

func TestInitializeDatabaseReplayKeepsExactPersistedAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "deployment.json")
	existing := deployment.Config{Database: deployment.Database{
		Host: "127.0.0.1", Port: 55432, Name: "dorf", User: "dorf", Password: "retained",
		Image: DatabaseImage, ImageID: DatabaseImageID, VolumeState: deployment.DatabaseVolumeInitialized,
	}, ControlReaderKey: strings.Repeat("a", 64), E2B: &deployment.E2B{APIKey: "retained-e2b"}}
	if err := deployment.Save(path, existing); err != nil {
		t.Fatal(err)
	}
	database, err := initializeDatabase(path, bytes.NewReader(bytes.Repeat([]byte{0xff}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if database != existing.Database {
		t.Fatalf("database=%#v want=%#v", database, existing.Database)
	}
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.E2B == nil || stored.E2B.APIKey != "retained-e2b" {
		t.Fatalf("stored=%#v found=%t error=%v", stored, found, err)
	}
}

func TestInitializeDatabaseBackfillsReaderKeyWithoutChangingExistingAuthority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "deployment.json")
	existing := deployment.Config{Database: deployment.Database{
		Host: "127.0.0.1", Port: 55432, Name: "dorf", User: "dorf", Password: "retained",
		Image: DatabaseImage, ImageID: DatabaseImageID, VolumeState: deployment.DatabaseVolumeInitialized,
	}, E2B: &deployment.E2B{APIKey: "retained-e2b"}}
	if err := deployment.Save(path, existing); err != nil {
		t.Fatal(err)
	}
	database, err := initializeDatabase(path, bytes.NewReader(bytes.Repeat([]byte{0xbc}, 32)))
	if err != nil || database != existing.Database {
		t.Fatalf("database=%#v error=%v", database, err)
	}
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.Database != existing.Database || stored.E2B == nil || stored.E2B.APIKey != "retained-e2b" || stored.ControlReaderKey != strings.Repeat("bc", 32) {
		t.Fatalf("stored=%#v found=%t error=%v", stored, found, err)
	}
	if _, err := initializeDatabase(path, failingReader{}); err != nil {
		t.Fatalf("replay regenerated reader key: %v", err)
	}
}

func TestInitializeDatabaseDoesNotPersistPartialCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	_, err := initializeDatabase(path, failingReader{})
	if err == nil || !errors.Is(err, errRandomUnavailable) {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial deployment persisted: %v", statErr)
	}
}

var errRandomUnavailable = errors.New("random unavailable")

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errRandomUnavailable }
