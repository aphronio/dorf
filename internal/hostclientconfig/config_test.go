package hostclientconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadProtectedHostClientConfiguration(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	path := Path(stateDir)
	want := Config{Credential: "opaque-host-secret", EnrollmentCode: "pending-enrollment"}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]os.FileMode{stateDir: 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != expected {
			t.Fatalf("%s mode=%o want=%o", name, info.Mode().Perm(), expected)
		}
	}
	want.EnrollmentCode = ""
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(contents, []byte("enrollment_code")) {
		t.Fatalf("completed configuration retained pending enrollment: %s", contents)
	}
	got, found, err := Load(path)
	if err != nil || !found || got != want {
		t.Fatalf("completed configuration=%#v found=%t err=%v", got, found, err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != filename {
		t.Fatalf("atomic save artifacts=%v err=%v", entries, err)
	}
}

func TestLoadFailsClosedForInsecureOrRedirectedHostClientConfiguration(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := Path(stateDir)
	contents := []byte(`{"credential":"do-not-report-this","enrollment_code":"pending"}`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("insecure file err=%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(stateDir, "target")
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || strings.Contains(err.Error(), "do-not-report-this") {
		t.Fatalf("linked file err=%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(contents, bytes.Repeat([]byte(" "), maximumFileSize)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("oversized configuration err=%v", err)
	}
}

func TestLoadRejectsMalformedStateWithoutLeakingCredential(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := Path(stateDir)
	for _, contents := range []string{
		`{"credential":""}`,
		`{"credential":"do-not-report-this","unknown":true}`,
		`{"credential":"do-not-report-this"}{}`,
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(path); err == nil || strings.Contains(err.Error(), "do-not-report-this") {
			t.Fatalf("Load(%q) err=%v", contents, err)
		}
	}
}
