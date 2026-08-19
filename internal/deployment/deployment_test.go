package deployment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestLoadRejectsIncompleteDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, []byte(`{"database":{"host":"127.0.0.1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err=%v", err)
	}
}
