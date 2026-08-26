package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

var (
	ErrInvalidFilePath = errors.New("invalid Sandbox file path")
	ErrFileUnavailable = errors.New("Sandbox file is unavailable")
)

// ValidateWorkspaceRelativePath accepts one exact Linux path beneath a
// Sandbox workspace. Discovery and directory semantics remain ordinary Exec
// concerns rather than growing this one-file API.
func ValidateWorkspaceRelativePath(relativePath string) error {
	if relativePath == "" || strings.IndexByte(relativePath, 0) >= 0 || path.IsAbs(relativePath) || path.Clean(relativePath) != relativePath || relativePath == "." || relativePath == ".." || strings.HasPrefix(relativePath, "../") {
		return fmt.Errorf("%w: path must be clean and workspace-relative", ErrInvalidFilePath)
	}
	return nil
}

// ReadFileViaExec returns exact regular-file bytes while refusing symlinks or
// any resolved path outside the canonical Sandbox workspace.
func ReadFileViaExec(ctx context.Context, owner Ownership, workspace, relativePath string, exec ExecFunc) ([]byte, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || !path.IsAbs(workspace) || path.Clean(workspace) != workspace || workspace == "/" {
		return nil, fmt.Errorf("Sandbox workspace must be a clean absolute path")
	}
	if err := ValidateWorkspaceRelativePath(relativePath); err != nil {
		return nil, err
	}
	if exec == nil {
		return nil, fmt.Errorf("Sandbox file transport is not configured")
	}
	script := `set -eu
workspace=$1 relative=$2
root=$(realpath -e -- "$workspace")
test -d "$root" && test ! -L "$workspace"
target="$root/$relative"
test -f "$target" && test ! -L "$target"
exec 3< "$target"
test -f /proc/self/fd/3
resolved=$(realpath -e -- /proc/self/fd/3)
test "$resolved" = "$target"
base64 -w0 <&3`
	result, err := exec(ctx, owner, nil, "bash", "-c", script, "dorf-read-file", workspace, relativePath)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("exit %d", result.ExitCode)
		}
		return nil, fmt.Errorf("%w: read regular workspace file %q: %s", ErrFileUnavailable, relativePath, detail)
	}
	contents, err := base64.StdEncoding.Strict().DecodeString(result.Stdout)
	if err != nil {
		return nil, fmt.Errorf("decode exact Sandbox workspace file %q: %w", relativePath, err)
	}
	return contents, nil
}

// ExecFunc is the bounded command transport used by provider adapters that do
// not expose a stronger native file API.
type ExecFunc func(context.Context, Ownership, []byte, ...string) (Result, error)

// PutFileViaExec reconciles one bounded byte sequence at an absolute regular
// file path. Bytes are written beside the destination, verified, then renamed
// atomically. Replaying after an indeterminate response is therefore safe.
func PutFileViaExec(ctx context.Context, owner Ownership, destination string, contents []byte, exec ExecFunc) error {
	destination = strings.TrimSpace(destination)
	if destination == "" || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination || destination == string(filepath.Separator) {
		return fmt.Errorf("Sandbox file destination must be a clean absolute path")
	}
	if exec == nil {
		return fmt.Errorf("Sandbox file transport is not configured")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(contents))
	script := `set -eu
destination=$1 expected_size=$2 expected_digest=$3
parent=$(dirname -- "$destination")
mkdir -p -- "$parent"
if test -f "$destination" && test ! -L "$destination" &&
   test "$(wc -c < "$destination")" = "$expected_size" &&
   test "$(sha256sum "$destination" | cut -d ' ' -f 1)" = "$expected_digest"; then
  exit 0
fi
if test -e "$destination" || test -L "$destination"; then
  test -f "$destination" && test ! -L "$destination"
fi
temporary="${destination}.dorf-new.$$"
trap 'rm -f -- "$temporary"' EXIT
umask 077
cat > "$temporary"
test "$(wc -c < "$temporary")" = "$expected_size"
test "$(sha256sum "$temporary" | cut -d ' ' -f 1)" = "$expected_digest"
chmod 600 "$temporary"
mv -f -- "$temporary" "$destination"
trap - EXIT`
	result, err := exec(ctx, owner, contents, "bash", "-c", script, "dorf-put-file", destination, strconv.Itoa(len(contents)), digest)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("put Sandbox file %s: %s", destination, strings.TrimSpace(result.Stderr))
	}
	return nil
}
