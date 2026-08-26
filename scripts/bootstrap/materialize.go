// Package bootstrap materializes reviewed host helpers. Dorf never runs them.
package bootstrap

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
)

type Name string

const Docker Name = "docker"
const Incus Name = "incus"
const RetireSystemd Name = "retire-systemd"

type Artifact struct{ Path, SHA256, Version string }

//go:embed docker.sh
var dockerScript []byte

//go:embed incus.sh
var incusScript []byte

//go:embed retire-systemd.sh
var retireSystemdScript []byte

var scripts = map[Name][]byte{Docker: dockerScript, Incus: incusScript, RetireSystemd: retireSystemdScript}
var releasePattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$`)

func Materialize(dataRoot, version string, name Name) (Artifact, error) {
	if !releasePattern.MatchString(version) {
		return Artifact{}, fmt.Errorf("invalid Dorf release %q", version)
	}
	script, ok := scripts[name]
	if !ok {
		return Artifact{}, fmt.Errorf("invalid bootstrap helper name %q", name)
	}

	root := filepath.Clean(dataRoot)
	if !filepath.IsAbs(root) {
		return Artifact{}, fmt.Errorf("bootstrap data root %q must be absolute", root)
	}
	rootInfo, rootErr := os.Lstat(root)
	if os.IsNotExist(rootErr) {
		parent := filepath.Dir(root)
		resolvedParent, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr != nil || resolvedParent != parent {
			return Artifact{}, fmt.Errorf("missing bootstrap data root %q needs a real existing parent", root)
		}
		if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
			return Artifact{}, err
		}
		rootInfo, rootErr = os.Lstat(root)
	}
	resolved, resolveErr := filepath.EvalSymlinks(root)
	if resolveErr != nil || resolved != root || rootErr != nil || rootInfo.Mode() != os.ModeDir|0o700 || !ownedByOperator(rootInfo) {
		return Artifact{}, fmt.Errorf("bootstrap data root %q must be a real protected 0700 directory", root)
	}
	sha := fmt.Sprintf("%x", sha256.Sum256(script))
	base := filepath.Join(root, "bootstrap")
	dir := filepath.Join(base, "v"+version, sha)
	for _, candidate := range []string{base, filepath.Dir(dir), dir} {
		if err := os.Mkdir(candidate, 0o700); err != nil && !os.IsExist(err) {
			return Artifact{}, err
		}
		info, err := os.Lstat(candidate)
		if err != nil || info.Mode() != os.ModeDir|0o700 || !ownedByOperator(info) {
			return Artifact{}, fmt.Errorf("bootstrap directory %q is not a protected operator-owned 0700 directory", candidate)
		}
	}

	path := filepath.Join(dir, string(name)+".sh")
	artifact := Artifact{Path: path, SHA256: sha, Version: version}
	if err := validateScript(path, script); err == nil {
		return artifact, nil
	} else if !os.IsNotExist(err) {
		return Artifact{}, err
	}
	temporary, err := os.CreateTemp(dir, ".helper-*.tmp")
	if err != nil {
		return Artifact{}, err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	if err := temporary.Chmod(0o700); err != nil {
		return Artifact{}, err
	}
	if _, err := temporary.Write(script); err != nil {
		return Artifact{}, err
	}
	if err := temporary.Sync(); err != nil {
		return Artifact{}, err
	}
	if err := temporary.Close(); err != nil {
		return Artifact{}, err
	}
	if err := os.Link(temporary.Name(), path); os.IsExist(err) {
		if err := validateScript(path, script); err != nil {
			return Artifact{}, err
		}
	} else if err != nil {
		return Artifact{}, fmt.Errorf("publish bootstrap helper without replacement: %w", err)
	}
	return artifact, nil
}

func validateScript(path string, want []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode() != 0o700 || !ownedByOperator(info) {
		return fmt.Errorf("bootstrap helper collision at %q differs from the canonical 0700 script", path)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		return fmt.Errorf("bootstrap helper collision at %q differs from the canonical 0700 script", path)
	}
	return nil
}

func ownedByOperator(info os.FileInfo) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(owner.Uid) == os.Geteuid() && int(owner.Gid) == os.Getegid()
}
