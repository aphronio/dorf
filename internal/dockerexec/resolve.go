// Package dockerexec resolves the one protected local Docker CLI that Dorf may execute.
package dockerexec

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	SystemPath = "/usr/bin/docker"
	LocalPath  = "/usr/local/bin/docker"
)

var acceptedPaths = [...]string{SystemPath, LocalPath}

// Resolve returns the one unambiguous protected Docker executable installed at
// an accepted system location. Every existing accepted candidate must be safe;
// Dorf never falls through from an unsafe or ambiguous executable authority.
func Resolve() (string, error) {
	return resolve(os.Lstat)
}

func resolve(lstat func(string) (os.FileInfo, error)) (string, error) {
	resolved := ""
	for _, path := range acceptedPaths {
		info, err := lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect Docker executable %s: %w", path, err)
		}
		owner, owned := info.Sys().(*syscall.Stat_t)
		mode := info.Mode()
		if !mode.IsRegular() || mode&os.ModeSymlink != 0 || !owned || owner == nil || owner.Uid != 0 || mode.Perm()&0o100 == 0 || mode.Perm()&0o022 != 0 {
			return "", fmt.Errorf("Docker executable %s must be one root-owned, non-symlink, protected owner-executable regular file", path)
		}
		if resolved != "" {
			return "", fmt.Errorf("Docker executable authority is ambiguous between %s and %s", resolved, path)
		}
		resolved = path
	}
	if resolved == "" {
		return "", fmt.Errorf("Docker executable is unavailable at %s or %s", SystemPath, LocalPath)
	}
	return resolved, nil
}
