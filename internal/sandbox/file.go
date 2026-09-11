package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

var (
	ErrInvalidFilePath = errors.New("invalid Sandbox file path")
	ErrFileUnavailable = errors.New("Sandbox file is unavailable")
)

// ValidateFilePath accepts an exact absolute, home-relative, or workspace-relative
// Linux file path. All resolution and file access happen inside the owned Sandbox.
func ValidateFilePath(name string) error {
	if name == "" || strings.IndexByte(name, 0) >= 0 || path.Clean(name) != name || name == "/" || name == "." || name == "~" || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("%w: path must name a clean Sandbox file", ErrInvalidFilePath)
	}
	return nil
}

const resolveFilePath = `workspace=$1 name=$2
case "$name" in
  /*) target=$name ;;
  '~/'*) target=~; target="$target/${name#\~/}" ;;
  *) root=$(realpath -e -- "$workspace"); test -d "$root" && test ! -L "$workspace"; target="$root/$name" ;;
esac
`

// ReadFileViaExec returns exact regular-file bytes while refusing symlinks.
func ReadFileViaExec(ctx context.Context, owner Ownership, workspace, relativePath string, exec ExecFunc) ([]byte, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" || !path.IsAbs(workspace) || path.Clean(workspace) != workspace || workspace == "/" {
		return nil, fmt.Errorf("Sandbox workspace must be a clean absolute path")
	}
	if err := ValidateFilePath(relativePath); err != nil {
		return nil, err
	}
	if exec == nil {
		return nil, fmt.Errorf("Sandbox file transport is not configured")
	}
	script := "set -eu\n" + resolveFilePath + `
if test ! -e "$target" && test ! -L "$target"; then exit 44; fi
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
		if result.ExitCode == 44 {
			return nil, fmt.Errorf("%w: %w: %s", ErrFileUnavailable, os.ErrNotExist, relativePath)
		}
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = fmt.Sprintf("exit %d", result.ExitCode)
		}
		return nil, fmt.Errorf("%w: read regular Sandbox file %q: %s", ErrFileUnavailable, relativePath, detail)
	}
	contents, err := base64.StdEncoding.Strict().DecodeString(result.Stdout)
	if err != nil {
		return nil, fmt.Errorf("decode exact Sandbox file %q: %w", relativePath, err)
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
	if !path.IsAbs(destination) {
		return fmt.Errorf("Sandbox file destination must be absolute")
	}
	return writeFileViaExec(ctx, owner, "", destination, contents, false, exec)
}

// MaxFileWriteBytes bounds an individual public Sandbox write.
const MaxFileWriteBytes = 8 << 20

// WriteFileViaExec atomically writes a Sandbox file, creating private parent directories.
// Create-only writes preserve existing files, including empty files.
func WriteFileViaExec(ctx context.Context, owner Ownership, workspace, name string, contents []byte, ifAbsent bool, exec ExecFunc) error {
	if len(contents) > MaxFileWriteBytes {
		return fmt.Errorf("Sandbox file exceeds write limit")
	}
	return writeFileViaExec(ctx, owner, workspace, name, contents, ifAbsent, exec)
}

func writeFileViaExec(ctx context.Context, owner Ownership, workspace, name string, contents []byte, ifAbsent bool, exec ExecFunc) error {
	if err := ValidateFilePath(name); err != nil {
		return err
	}
	if exec == nil {
		return fmt.Errorf("Sandbox file transport is not configured")
	}
	if !path.IsAbs(name) && !strings.HasPrefix(name, "~/") && (!path.IsAbs(workspace) || path.Clean(workspace) != workspace || workspace == "/") {
		return fmt.Errorf("invalid Sandbox workspace")
	}

	script := "set -eu\n" + resolveFilePath + `
expected_size=$3 expected_digest=$4 if_absent=$5
parent=$(dirname -- "$target")
test "$(realpath -m -- "$parent")" = "$parent"
umask 077
mkdir -p -- "$parent"
cd -- "$parent"
name=$(basename -- "$target")
if test -e "$name" || test -L "$name"; then
  test -f "$name" && test ! -L "$name"
  if test "$if_absent" = true; then exit 0; fi
fi
umask 077
temporary=$(mktemp .dorf-write.XXXXXXXX)
trap 'rm -f -- "$temporary"' EXIT
cat > "$temporary"
test "$(wc -c < "$temporary")" = "$expected_size"
test "$(sha256sum "$temporary" | cut -d ' ' -f 1)" = "$expected_digest"
if test "$if_absent" = true; then
  if ! ln -T -- "$temporary" "$name"; then test -f "$name" && test ! -L "$name"; fi
else
  mv -fT -- "$temporary" "$name"
fi`
	result, err := exec(ctx, owner, contents, "bash", "-c", script, "dorf-write-file", workspace, name, strconv.Itoa(len(contents)), fmt.Sprintf("%x", sha256.Sum256(contents)), strconv.FormatBool(ifAbsent))
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%w: Sandbox file write failed", ErrFileUnavailable)
	}
	return nil
}
