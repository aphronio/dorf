package sandbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

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
