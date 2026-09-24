package codex

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPersistenceCopyIsIndependentAndPreservesSelection(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "live")
	copy := filepath.Join(root, "saved")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kept", "ignored.sqlite-shm", "nested/kept.sqlite-shm"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("kept", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	paths, _ := json.Marshal([]string{source})
	excludes, _ := json.Marshal([]string{source + "/*.sqlite-shm"})
	result, err := exec.Command("python3", "-c", copyPersistenceTree, copy, string(paths), string(excludes)).CombinedOutput()
	if err != nil {
		t.Fatalf("copy: %v: %s", err, result)
	}
	if err := os.WriteFile(filepath.Join(source, "kept"), []byte("after"), 0600); err != nil {
		t.Fatal(err)
	}
	saved := copy + source
	if data, err := os.ReadFile(filepath.Join(saved, "kept")); err != nil || string(data) != "before" {
		t.Fatal("live write changed saved bytes")
	}
	if _, err := os.Stat(filepath.Join(saved, "ignored.sqlite-shm")); !os.IsNotExist(err) {
		t.Fatal("excluded operational file was saved")
	}
	if _, err := os.Stat(filepath.Join(saved, "nested/kept.sqlite-shm")); err != nil {
		t.Fatal("exclusion escaped its native root")
	}
	if target, err := os.Readlink(filepath.Join(saved, "link")); err != nil || target != "kept" {
		t.Fatal("symbolic link was followed")
	}
}
