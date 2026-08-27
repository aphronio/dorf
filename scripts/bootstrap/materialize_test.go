package bootstrap

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOwnedByOperatorRejectsDifferentIdentity(t *testing.T) {
	info, err := os.Lstat(protectedTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	owner := *info.Sys().(*syscall.Stat_t)
	owner.Uid++
	if ownedByOperator(fileInfoWithOwner{FileInfo: info, owner: &owner}) {
		t.Fatal("different owner was accepted")
	}
}

func TestMaterializePublishesExactVersionedHelpers(t *testing.T) {
	for _, test := range []struct {
		name   Name
		script []byte
	}{
		{Docker, dockerScript},
		{Incus, incusScript},
		{IncusRemote, incusRemoteScript},
	} {
		t.Run(string(test.name), func(t *testing.T) {
			root := filepath.Join(protectedTempDir(t), "dorf-data")
			first, err := Materialize(root, "0.5.2", test.name)
			if err != nil {
				t.Fatalf("Materialize() error = %v", err)
			}
			digest := sha256.Sum256(test.script)
			sha := hex.EncodeToString(digest[:])
			wantPath := filepath.Join(root, "bootstrap", "v0.5.2", sha, string(test.name)+".sh")
			if first != (Artifact{Path: wantPath, SHA256: sha, Version: "0.5.2"}) {
				t.Fatalf("artifact = %#v", first)
			}
			got, err := os.ReadFile(first.Path)
			if err != nil || string(got) != string(test.script) {
				t.Fatalf("materialized bytes differ: %v", err)
			}
			assertMode(t, first.Path, 0o700)
			assertMode(t, filepath.Dir(first.Path), 0o700)
			assertMode(t, root, 0o700)
			assertCurrentOwner(t, root)
			assertCurrentOwner(t, first.Path)
			second, err := Materialize(root, "0.5.2", test.name)
			if err != nil || second != first {
				t.Fatalf("replay = %#v, %v; want %#v", second, err, first)
			}
		})
	}
}

func TestMaterializeRejectsInvalidIdentityAndCollisions(t *testing.T) {
	root := protectedTempDir(t)
	for _, version := range []string{"", "v0.5.2", "0.5", "00.5.2", "0.5.2/escape", "0.5.2+local"} {
		if _, err := Materialize(root, version, Docker); err == nil || !strings.Contains(err.Error(), "release") {
			t.Errorf("version %q error = %v", version, err)
		}
	}
	for _, name := range []Name{"", "Docker", "docker.sh", "../docker", "cloudflare"} {
		if _, err := Materialize(root, "0.5.2", name); err == nil || !strings.Contains(err.Error(), "name") {
			t.Errorf("name %q error = %v", name, err)
		}
	}

	artifact, err := Materialize(root, "0.5.2", Docker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact.Path, []byte("changed\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(root, "0.5.2", Docker); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("content collision error = %v", err)
	}
}

func TestMaterializeRejectsUnprotectedOrSymlinkedCustody(t *testing.T) {
	t.Run("root mode", func(t *testing.T) {
		root := protectedTempDir(t)
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Materialize(root, "0.5.2", Docker); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("root symlink", func(t *testing.T) {
		parent := protectedTempDir(t)
		realRoot := filepath.Join(parent, "real")
		if err := os.Mkdir(realRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(parent, "alias")
		if err := os.Symlink(realRoot, alias); err != nil {
			t.Fatal(err)
		}
		if _, err := Materialize(alias, "0.5.2", Docker); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("owned component", func(t *testing.T) {
		root := protectedTempDir(t)
		outside := protectedTempDir(t)
		if err := os.Symlink(outside, filepath.Join(root, "bootstrap")); err != nil {
			t.Fatal(err)
		}
		if _, err := Materialize(root, "0.5.2", Docker); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Fatalf("error = %v", err)
		}
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Fatalf("symlink target was changed: %v", entries)
		}
	})

	t.Run("fifo collision", func(t *testing.T) {
		root := protectedTempDir(t)
		artifact, err := Materialize(root, "0.5.2", Docker)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(artifact.Path); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(artifact.Path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := Materialize(root, "0.5.2", Docker); err == nil || !strings.Contains(err.Error(), "collision") {
			t.Fatalf("FIFO collision error = %v", err)
		}
	})
}

func protectedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("mode(%q) = %#o, want %#o", path, info.Mode().Perm(), want)
	}
}

func assertCurrentOwner(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid() {
		t.Fatalf("owner(%q) is not current %d:%d", path, os.Geteuid(), os.Getegid())
	}
}

type fileInfoWithOwner struct {
	os.FileInfo
	owner *syscall.Stat_t
}

func (info fileInfoWithOwner) Sys() any { return info.owner }
