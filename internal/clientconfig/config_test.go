package clientconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveLoadRemoveProtectedClientConfiguration(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "dorf")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, filename)
	want := Config{DeploymentURL: "https://dorf.example.test/", Credential: "opaque-client-secret"}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	for name, expected := range map[string]os.FileMode{directory: 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != expected {
			t.Fatalf("%s mode=%o want=%o", name, info.Mode().Perm(), expected)
		}
	}
	got, found, err := Load(path)
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	if got.DeploymentURL != "https://dorf.example.test" || got.Credential != want.Credential {
		t.Fatalf("configuration=%#v", got)
	}
	if err := Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, found, err := Load(path); err != nil || found {
		t.Fatalf("removed configuration found=%t err=%v", found, err)
	}
	if err := Remove(path); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
}

func TestLoadFailsClosedForInsecureOrRedirectedCredentialFiles(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "dorf")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, filename)
	contents := []byte(`{"deployment_url":"https://dorf.example.test","credential":"do-not-report-this"}`)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("insecure file err=%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || strings.Contains(err.Error(), "do-not-report-this") {
		t.Fatalf("linked file err=%v", err)
	}
	if err := Remove(path); err == nil {
		t.Fatal("Remove followed a symbolic link")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(contents, bytes.Repeat([]byte(" "), 64<<10)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("oversized configuration err=%v", err)
	}
}

func TestDeploymentURLRejectsCredentialBearingOrInexactURLs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "relative")
	if got := Path("/home/operator"); got != "/home/operator/.config/dorf/client.json" {
		t.Fatalf("relative XDG path was used: %s", got)
	}
	for _, raw := range []string{
		"http://dorf.example.test",
		"https://user:secret@dorf.example.test",
		"https://dorf.example.test?credential=secret",
		"https://dorf.example.test/#secret",
		"https://dorf.example.test/control",
		"https:///missing-host",
	} {
		if _, err := NormalizeDeploymentURL(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("NormalizeDeploymentURL(%q) err=%v", raw, err)
		}
	}
}
