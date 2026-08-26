package deployment

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSaveLoadDockerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dorf", "deployment.json")
	want := Config{Database: Database{Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret", Image: "postgres:17.10-bookworm", ImageID: "sha256:exact"}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o want=600", info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode() != os.ModeDir|0o700 {
		t.Fatalf("directory mode=%v want=drwx------", directoryInfo.Mode())
	}
	got, found, err := Load(path)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got != want {
		t.Fatalf("got=%#v want=%#v", got, want)
	}
	url, err := got.Database.URL()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(url, "dorf:secret@127.0.0.1:54329/dorf") || !strings.Contains(url, "sslmode=disable") {
		t.Fatalf("URL=%q", url)
	}
}

func TestLoadMissingDeploymentRemainsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created", "dorf", "deployment.json")
	got, found, err := Load(path)
	if err != nil || found || got != (Config{}) {
		t.Fatalf("config=%#v found=%t error=%v", got, found, err)
	}
}

func TestDeploymentPathMustBeOneCleanAbsoluteFile(t *testing.T) {
	for _, path := range []string{
		"deployment.json",
		t.TempDir() + string(filepath.Separator) + "dorf" + string(filepath.Separator) + ".." + string(filepath.Separator) + "deployment.json",
		string(filepath.Separator),
	} {
		if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "clean absolute path") {
			t.Fatalf("Load(%q) error=%v", path, err)
		}
		if err := Save(path, Config{Database: testDatabase()}); err == nil || !strings.Contains(err.Error(), "clean absolute path") {
			t.Fatalf("Save(%q) error=%v", path, err)
		}
	}
}

func TestLoadRejectsDeploymentFileSymlink(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(directory, "real.json")
	if err := Save(realPath, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := os.Symlink(realPath, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "real operator-owned regular file") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadRejectsPermissiveDeploymentFile(t *testing.T) {
	path := protectedDeploymentTestPath(t)
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "mode 0600") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadRejectsDeploymentDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	path := filepath.Join(realDirectory, "deployment.json")
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(filepath.Join(linkedDirectory, "deployment.json")); err == nil || !strings.Contains(err.Error(), "real operator-owned directory") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadRejectsPermissiveDeploymentDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "mode 0700") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadRejectsSpecialDeploymentFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadRejectsForeignOwnedDeploymentFileWhenOwnershipCanBeChanged(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing file ownership requires root")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "operator-owned") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadRejectsOversizeDeploymentFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := os.WriteFile(path, make([]byte, (64<<10)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestSaveRejectsOversizeDeploymentConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dorf", "deployment.json")
	err := Save(path, Config{Database: testDatabase(), E2B: &E2B{APIKey: strings.Repeat("x", 64<<10)}})
	if err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("Save() error=%v", err)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("oversize configuration unexpectedly written: %v", statErr)
	}
}

func TestLoadRejectsDeploymentFileReplacementRace(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	replacement := filepath.Join(directory, "replacement.json")
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	changed := testDatabase()
	changed.Password = "replaced"
	if err := Save(replacement, Config{Database: changed}); err != nil {
		t.Fatal(err)
	}
	deploymentLoadOpenedForTest = func() {
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { deploymentLoadOpenedForTest = nil })
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "changed while it was read") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadRejectsDeploymentDirectoryReplacementRace(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "dorf")
	path := filepath.Join(directory, "deployment.json")
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "moved")
	deploymentLoadOpenedForTest = func() {
		if err := os.Rename(directory, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { deploymentLoadOpenedForTest = nil })
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "directory changed") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestSaveRejectsDeploymentFileSymlinkWithoutChangingReferent(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	referent := filepath.Join(directory, "referent")
	if err := os.WriteFile(referent, []byte("do not replace"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := os.Symlink(referent, path); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, Config{Database: testDatabase()}); err == nil || !strings.Contains(err.Error(), "real operator-owned regular file") {
		t.Fatalf("Save() error=%v", err)
	}
	contents, err := os.ReadFile(referent)
	if err != nil || string(contents) != "do not replace" {
		t.Fatalf("referent=%q error=%v", contents, err)
	}
}

func TestSaveRejectsInsecureExistingDeploymentCustody(t *testing.T) {
	t.Run("permissive directory", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o750); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "deployment.json")
		if err := Save(path, Config{Database: testDatabase()}); err == nil || !strings.Contains(err.Error(), "mode 0700") {
			t.Fatalf("Save() error=%v", err)
		}
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o750 {
			t.Fatalf("directory mode=%v", info.Mode())
		}
	})

	t.Run("permissive file", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "deployment.json")
		if err := os.WriteFile(path, []byte("insecure"), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := Save(path, Config{Database: testDatabase()}); err == nil || !strings.Contains(err.Error(), "mode 0600") {
			t.Fatalf("Save() error=%v", err)
		}
	})

	t.Run("linked directory", func(t *testing.T) {
		root := t.TempDir()
		realDirectory := filepath.Join(root, "real")
		if err := os.Mkdir(realDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		linkedDirectory := filepath.Join(root, "linked")
		if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(linkedDirectory, "deployment.json")
		if err := Save(path, Config{Database: testDatabase()}); err == nil || !strings.Contains(err.Error(), "real operator-owned directory") {
			t.Fatalf("Save() error=%v", err)
		}
		if _, err := os.Lstat(filepath.Join(realDirectory, "deployment.json")); !os.IsNotExist(err) {
			t.Fatalf("linked target unexpectedly written: %v", err)
		}
	})
}

func TestSaveRejectsDeploymentFileReplacementRace(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	replacement := filepath.Join(directory, "replacement.json")
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	racer := testDatabase()
	racer.Password = "racer"
	if err := Save(replacement, Config{Database: racer}); err != nil {
		t.Fatal(err)
	}
	deploymentSaveBeforeCommitForTest = func() {
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { deploymentSaveBeforeCommitForTest = nil })
	wanted := testDatabase()
	wanted.Password = "wanted"
	if err := Save(path, Config{Database: wanted}); err == nil || !strings.Contains(err.Error(), "changed while it was being saved") {
		t.Fatalf("Save() error=%v", err)
	}
	deploymentSaveBeforeCommitForTest = nil
	got, found, err := Load(path)
	if err != nil || !found || got.Database.Password != "racer" {
		t.Fatalf("config=%#v found=%t error=%v", got, found, err)
	}
}

func TestSaveRejectsTemporaryReplacementWithoutLosingCurrentConfig(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	deploymentSaveBeforeCommitForTest = func() {
		matches, err := filepath.Glob(filepath.Join(directory, ".deployment-*.json"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("temporary files=%v error=%v", matches, err)
		}
		if err := os.Remove(matches[0]); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, matches[0]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { deploymentSaveBeforeCommitForTest = nil })
	wanted := testDatabase()
	wanted.Password = "wanted"
	if err := Save(path, Config{Database: wanted}); err == nil || !strings.Contains(err.Error(), "temporary Dorf deployment configuration changed") {
		t.Fatalf("Save() error=%v", err)
	}
	deploymentSaveBeforeCommitForTest = nil
	got, found, err := Load(path)
	if err != nil || !found || got.Database != testDatabase() {
		t.Fatalf("config=%#v found=%t error=%v", got, found, err)
	}
}

func TestLoadRejectsIncompleteDatabase(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "deployment.json")
	if err := os.WriteFile(path, []byte(`{"database":{"host":"127.0.0.1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err=%v", err)
	}
}

func TestSaveE2BAPIKeyPreservesDatabaseAndSupportsRotation(t *testing.T) {
	path := protectedDeploymentTestPath(t)
	database := Database{Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret", Image: "postgres:17.10-bookworm", ImageID: "sha256:exact"}
	if err := Save(path, Config{Database: database}); err != nil {
		t.Fatal(err)
	}
	if err := SaveE2BAPIKey(path, "e2b-secret"); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load(path)
	if err != nil || !found || got.Database != database || got.E2B == nil || got.E2B.APIKey != "e2b-secret" {
		t.Fatalf("config=%#v found=%t err=%v", got, found, err)
	}
	if err := SaveE2BAPIKey(path, "e2b-secret"); err != nil {
		t.Fatalf("idempotent save: %v", err)
	}
	if err := SaveE2BAPIKey(path, "different"); err != nil {
		t.Fatal(err)
	}
	rotated, _, err := Load(path)
	if err != nil || rotated.Database != database || rotated.E2B == nil || rotated.E2B.APIKey != "different" {
		t.Fatalf("rotated=%#v err=%v", rotated, err)
	}
}

func TestEnsureControlReaderKeyIsValidatedAndSetOnce(t *testing.T) {
	path := protectedDeploymentTestPath(t)
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureControlReaderKey(path, "not-a-key"); err == nil || !strings.Contains(err.Error(), "256-bit lowercase hex") {
		t.Fatalf("invalid key error=%v", err)
	}
	first := strings.Repeat("a", 64)
	if err := EnsureControlReaderKey(path, first); err != nil {
		t.Fatal(err)
	}
	if err := EnsureControlReaderKey(path, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load(path)
	if err != nil || !found || got.ControlReaderKey != first {
		t.Fatalf("config=%#v found=%t error=%v", got, found, err)
	}
}

func TestDeploymentUpdatesSerializeWholeReadModifyWrite(t *testing.T) {
	path := protectedDeploymentTestPath(t)
	if err := Save(path, Config{Database: testDatabase()}); err != nil {
		t.Fatal(err)
	}
	claimed := make(chan struct{}, 1)
	firstLoaded := make(chan struct{})
	releaseFirst := make(chan struct{})
	deploymentUpdateLoadedForTest = func() {
		select {
		case claimed <- struct{}{}:
			close(firstLoaded)
			<-releaseFirst
		default:
		}
	}
	t.Cleanup(func() { deploymentUpdateLoadedForTest = nil })

	firstDone := make(chan error, 1)
	go func() { firstDone <- SaveE2BAPIKey(path, "e2b-secret") }()
	<-firstLoaded
	secondDone := make(chan error, 1)
	go func() { secondDone <- SaveIncus(path, Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}) }()
	var secondErr error
	secondFinished := false
	select {
	case secondErr = <-secondDone:
		secondFinished = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if !secondFinished {
		secondErr = <-secondDone
	}
	if secondErr != nil {
		t.Fatal(secondErr)
	}
	deploymentUpdateLoadedForTest = nil
	got, found, err := Load(path)
	if err != nil || !found || got.E2B == nil || got.E2B.APIKey != "e2b-secret" || got.Incus == nil {
		t.Fatalf("config=%#v found=%t error=%v", got, found, err)
	}
}

func TestMarkDatabaseVolumeInitializedIsExactAndIdempotent(t *testing.T) {
	path := protectedDeploymentTestPath(t)
	database := Database{
		Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret",
		Image: "postgres:17.10-bookworm", ImageID: "sha256:exact", VolumeState: DatabaseVolumePending,
	}
	if err := Save(path, Config{Database: database, E2B: &E2B{APIKey: "retained"}, Incus: &Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}}); err != nil {
		t.Fatal(err)
	}
	if err := MarkDatabaseVolumeInitialized(path, database); err != nil {
		t.Fatal(err)
	}
	if err := MarkDatabaseVolumeInitialized(path, database); err != nil {
		t.Fatalf("idempotent mark: %v", err)
	}
	got, found, err := Load(path)
	if err != nil || !found || got.Database.VolumeState != DatabaseVolumeInitialized || got.E2B == nil || got.E2B.APIKey != "retained" ||
		got.Incus == nil || got.Incus.Endpoint != "unix:///var/lib/incus/unix.socket" {
		t.Fatalf("config=%#v found=%t err=%v", got, found, err)
	}
	changed := database
	changed.Password = "different"
	if err := MarkDatabaseVolumeInitialized(path, changed); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed authority error=%v", err)
	}
}

func TestDatabaseRejectsUnknownVolumeState(t *testing.T) {
	database := Database{
		Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret",
		Image: "postgres:17.10-bookworm", ImageID: "sha256:exact", VolumeState: "invented",
	}
	if err := database.Validate(); err == nil || !strings.Contains(err.Error(), "volume state") {
		t.Fatalf("Validate() error=%v", err)
	}
}

func TestIncusDeploymentRoundTripsOneExplicitLocalAuthority(t *testing.T) {
	path := protectedDeploymentTestPath(t)
	want := Config{
		Database: testDatabase(),
		Incus:    &Incus{Endpoint: "unix:///var/lib/incus/unix.socket"},
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load(path)
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("config=%#v found=%t err=%v", got, found, err)
	}
	hash, err := got.Incus.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	if hash != "5579362efc9a20ad35d2e6aebe39af24a054e2ff49fdca1441cf67c56fcc9b2a" {
		t.Fatalf("local Incus authority hash=%q", hash)
	}
}

func TestIncusDeploymentRemoteAuthorityHashExcludesPrivateKey(t *testing.T) {
	serverCertificate, _ := deploymentTestCertificate(t, "incus.example")
	clientCertificate, clientPrivateKey := deploymentTestCertificate(t, "dorf-worker")
	remote := Incus{
		Endpoint:          "https://incus.example:8443",
		ServerCertificate: serverCertificate,
		ClientCertificate: clientCertificate,
		ClientPrivateKey:  clientPrivateKey,
	}
	hash, err := remote.AuthorityHash()
	if err != nil {
		t.Fatal(err)
	}
	if len(hash) != 64 || strings.Contains(hash, strings.TrimSpace(clientPrivateKey)) {
		t.Fatalf("unsafe or malformed authority hash=%q", hash)
	}
	rotatedKey := remote
	_, rotatedKey.ClientPrivateKey = deploymentTestCertificate(t, "unrelated-key")
	if rotatedHash, err := rotatedKey.AuthorityHash(); err == nil || rotatedHash != "" {
		t.Fatalf("mismatched private key hash=%q err=%v", rotatedHash, err)
	}
	formatted := remote
	formatted.ServerCertificate = "\n" + remote.ServerCertificate + "\n"
	formatted.ClientCertificate = "\n" + remote.ClientCertificate + "\n"
	formatted.ClientPrivateKey = "\n" + remote.ClientPrivateKey + "\n"
	formattedHash, err := formatted.AuthorityHash()
	if err != nil || formattedHash != hash {
		t.Fatalf("PEM formatting changed public authority: hash=%q err=%v", formattedHash, err)
	}
}

func TestIncusDeploymentRejectsAmbiguousOrIncompleteAuthorities(t *testing.T) {
	serverCertificate, _ := deploymentTestCertificate(t, "incus.example")
	clientCertificate, clientPrivateKey := deploymentTestCertificate(t, "dorf-worker")
	validRemote := Incus{
		Endpoint:          "https://incus.example:8443",
		ServerCertificate: serverCertificate,
		ClientCertificate: clientCertificate,
		ClientPrivateKey:  clientPrivateKey,
	}
	for _, test := range []struct {
		name   string
		value  Incus
		detail string
	}{
		{name: "ambient", value: Incus{}, detail: "endpoint"},
		{name: "relative unix", value: Incus{Endpoint: "unix://relative/socket"}, detail: "absolute"},
		{name: "unclean unix", value: Incus{Endpoint: "unix:///var/lib/incus/../incus/unix.socket"}, detail: "canonical"},
		{name: "unix with TLS", value: Incus{Endpoint: "unix:///var/lib/incus/unix.socket", ClientPrivateKey: clientPrivateKey}, detail: "TLS"},
		{name: "HTTP", value: Incus{Endpoint: "http://incus.example:8443"}, detail: "unix or HTTPS"},
		{name: "remote path", value: Incus{Endpoint: "https://incus.example:8443/1.0"}, detail: "origin"},
		{name: "remote query", value: Incus{Endpoint: "https://incus.example:8443?project=default"}, detail: "origin"},
		{name: "missing server pin", value: func() Incus { c := validRemote; c.ServerCertificate = ""; return c }(), detail: "server certificate"},
		{name: "missing client certificate", value: func() Incus { c := validRemote; c.ClientCertificate = ""; return c }(), detail: "client certificate"},
		{name: "missing client key", value: func() Incus { c := validRemote; c.ClientPrivateKey = ""; return c }(), detail: "client private key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.value.Validate(); err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.detail)) {
				t.Fatalf("Validate() error=%v, want %q", err, test.detail)
			}
		})
	}
}

func TestSaveIncusPreservesExistingDeploymentAuthority(t *testing.T) {
	path := protectedDeploymentTestPath(t)
	if err := Save(path, Config{Database: testDatabase(), E2B: &E2B{APIKey: "retained"}}); err != nil {
		t.Fatal(err)
	}
	want := Incus{Endpoint: "unix:///var/lib/incus/unix.socket"}
	if err := SaveIncus(path, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := Load(path)
	if err != nil || !found || got.Database != testDatabase() || got.E2B == nil || got.E2B.APIKey != "retained" || got.Incus == nil || *got.Incus != want {
		t.Fatalf("config=%#v found=%t err=%v", got, found, err)
	}
}

func testDatabase() Database {
	return Database{Host: "127.0.0.1", Port: 54329, Name: "dorf", User: "dorf", Password: "secret", Image: "postgres:17.10-bookworm", ImageID: "sha256:exact"}
}

func protectedDeploymentTestPath(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(directory, "deployment.json")
}

func deploymentTestCertificate(t *testing.T, commonName string) (string, string) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Unix(1_700_000_000, 0),
		NotAfter:     time.Unix(2_000_000_000, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return string(certificate), string(key)
}
