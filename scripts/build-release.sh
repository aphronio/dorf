#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
OUTPUT_DIR="${1:-$PROJECT_ROOT/dist/release}"
VERSION="$(go -C "$PROJECT_ROOT" run ./cmd/dorf version | awk '{print $2}')"
ARCHIVE="dorf_${VERSION}_linux_x86_64.tar.gz"
INSTALLER="$OUTPUT_DIR/install.sh"
STAGE="$(mktemp -d)"
cleanup() { rm -rf -- "$STAGE"; }
trap cleanup EXIT

mkdir -p "$OUTPUT_DIR"
sed "s/@DORF_VERSION@/v$VERSION/g" "$PROJECT_ROOT/scripts/install.sh" >"$INSTALLER"
chmod 0755 "$INSTALLER"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go -C "$PROJECT_ROOT" build -trimpath -ldflags='-s -w' -o "$STAGE/dorf" ./cmd/dorf
install -m 0644 "$PROJECT_ROOT/LICENSE" "$STAGE/LICENSE"
tar -C "$STAGE" -czf "$OUTPUT_DIR/$ARCHIVE" dorf LICENSE
(
  cd "$OUTPUT_DIR"
  sha256sum "$ARCHIVE" >"dorf_${VERSION}_checksums.txt"
)
printf 'Go release ready: %s\n' "$OUTPUT_DIR/$ARCHIVE"
