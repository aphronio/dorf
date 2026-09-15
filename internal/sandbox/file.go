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
	ErrFileTooLarge    = errors.New("Sandbox file exceeds read limit")
)

const (
	// MaxFileReadBytes is the per-call size contract for Sandbox file reads.
	MaxFileReadBytes = 8 << 20
	// MaxConcurrentFileReads bounds retained file-response buffers at each HTTP hop.
	MaxConcurrentFileReads = 4
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

// FileBatchReader optionally reads a bounded set of files in one provider
// operation. Missing paths are absent from the result; empty files are present.
// Other failures reject the entire batch. Files need not form an atomic snapshot.
type FileBatchReader interface {
	ReadFiles(context.Context, Ownership, []string, int) (map[string][]byte, error)
}

// ReadFilesViaExec preserves ReadFileViaExec's regular-file and descriptor checks
// while bounding each read before its bytes cross the provider transport.
func ReadFilesViaExec(ctx context.Context, owner Ownership, workspace string, names []string, maxBytes int, exec ExecFunc) (map[string][]byte, error) {
	if len(names) == 0 || len(names) > 32 || maxBytes <= 0 || maxBytes > MaxFileWriteBytes/len(names) {
		return nil, fmt.Errorf("invalid Sandbox file batch bounds")
	}
	if workspace == "" || !path.IsAbs(workspace) || path.Clean(workspace) != workspace || workspace == "/" {
		return nil, fmt.Errorf("Sandbox workspace must be a clean absolute path")
	}
	for _, name := range names {
		if err := ValidateFilePath(name); err != nil {
			return nil, err
		}
	}
	if exec == nil {
		return nil, fmt.Errorf("Sandbox file transport is not configured")
	}
	script := "set -euo pipefail\nread_one() {\n" + resolveFilePath + `
if test ! -e "$target" && test ! -L "$target"; then printf 'M\n'; return; fi
test -f "$target" && test ! -L "$target"
exec 3< "$target"
test -f /proc/self/fd/3
resolved=$(realpath -e -- /proc/self/fd/3)
test "$resolved" = "$target"
printf D
head -c "$limit" <&3 | base64 -w0
printf '\n'
exec 3<&-
}
root_workspace=$1 limit=$2
shift 2
for name do read_one "$root_workspace" "$name"; done`
	args := append([]string{"bash", "-c", script, "dorf-read-files", workspace, strconv.Itoa(maxBytes + 1)}, names...)
	result, err := exec(ctx, owner, nil, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("%w: read regular Sandbox file batch (exit %d)", ErrFileUnavailable, result.ExitCode)
	}
	return decodeFileBatch(result.Stdout, names, maxBytes)
}

func decodeFileBatch(output string, names []string, maxBytes int) (map[string][]byte, error) {
	maxEncodedBytes := base64.StdEncoding.EncodedLen(maxBytes + 1)
	if len(output) > len(names)*(maxEncodedBytes+2) {
		return nil, fmt.Errorf("Sandbox file batch response exceeds limit")
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != len(names) || !strings.HasSuffix(output, "\n") {
		return nil, fmt.Errorf("incomplete Sandbox file batch response")
	}
	files := make(map[string][]byte, len(names))
	for i, line := range lines {
		if line == "M" {
			continue
		}
		if !strings.HasPrefix(line, "D") {
			return nil, fmt.Errorf("invalid Sandbox file batch entry")
		}
		encoded := line[1:]
		if len(encoded) > maxEncodedBytes {
			return nil, fmt.Errorf("Sandbox file %q response exceeds encoded limit", names[i])
		}
		contents, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode exact Sandbox file %q: %w", names[i], err)
		}
		if len(contents) > maxBytes {
			return nil, fmt.Errorf("%w: %q", ErrFileTooLarge, names[i])
		}
		files[names[i]] = contents
	}
	return files, nil
}

// ReadFileViaExec returns at most MaxFileReadBytes exact regular-file bytes
// while refusing symlinks.
func ReadFileViaExec(ctx context.Context, owner Ownership, workspace, relativePath string, exec ExecFunc) ([]byte, error) {
	files, err := ReadFilesViaExec(ctx, owner, strings.TrimSpace(workspace), []string{relativePath}, MaxFileReadBytes, exec)
	if err != nil {
		return nil, err
	}
	contents, ok := files[relativePath]
	if !ok {
		return nil, fmt.Errorf("%w: %w: %s", ErrFileUnavailable, os.ErrNotExist, relativePath)
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
