#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${PROVIDER_CONNECTION:-}" ]]; then
  echo "Set PROVIDER_CONNECTION to one connected Provider Gateway name." >&2
  exit 2
fi

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
OUTPUT_DIR="${OUTPUT_DIR:-$PROJECT_ROOT/dist/room-image}"
NETWORK="${NETWORK:-incusbr0}"
ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
SOURCE_COMMIT="${SOURCE_COMMIT:-$(git -C "$PROJECT_ROOT" rev-parse HEAD)}"
BUILD_ID="$(date -u +%Y%m%d%H%M%S)"
CANDIDATE_ALIAS="dorf-codex-candidate-$BUILD_ID"
BUILD_VM="dorf-codex-build-$BUILD_ID"
PROBE_VM="dorf-codex-probe-$BUILD_ID"
METADATA_PATH="$OUTPUT_DIR/image.json"
ARCHIVE_PATH="$OUTPUT_DIR/dorf-codex-x86_64.tar.gz"

uv run --project "$PROJECT_ROOT" python \
  "$SCRIPT_DIR/validate-dorf-codex-image.py" \
  --provider-connection "$PROVIDER_CONNECTION" \
  --network "$NETWORK" \
  --preflight-only

cleanup() {
  if incus info "$PROBE_VM" >/dev/null 2>&1; then
    incus delete "$PROBE_VM" --force >/dev/null 2>&1 || true
  fi
  if incus image info "$CANDIDATE_ALIAS" >/dev/null 2>&1; then
    incus image delete "$CANDIDATE_ALIAS" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

mkdir -p "$OUTPUT_DIR"
rm -f "$METADATA_PATH" "$ARCHIVE_PATH" \
  "$OUTPUT_DIR/dorf-codex-x86_64.json"

IMAGE_ALIAS="$CANDIDATE_ALIAS" \
BUILD_VM="$BUILD_VM" \
NETWORK="$NETWORK" \
ROOT_DISK_SIZE="$ROOT_DISK_SIZE" \
IMAGE_METADATA_PATH="$METADATA_PATH" \
  "$SCRIPT_DIR/build-dorf-codex-image.sh"

incus launch "$CANDIDATE_ALIAS" "$PROBE_VM" --vm --network "$NETWORK"
for _ in {1..60}; do
  if incus exec "$PROBE_VM" -- true >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
incus exec "$PROBE_VM" -- bash -lc '
  test ! -e /root/.codex/auth.json
  test ! -e /root/.codex/config.toml
  test ! -e /root/.config/dorf/provider-route.key
  test -z "${OPENAI_API_KEY:-}"
  test -x "$(command -v codex)"
  test -x "$(command -v uv)"
  test -r /usr/local/share/dorf/image.json
'
incus delete "$PROBE_VM" --force

uv run --project "$PROJECT_ROOT" python \
  "$SCRIPT_DIR/validate-dorf-codex-image.py" \
  --image "$CANDIDATE_ALIAS" \
  --provider-connection "$PROVIDER_CONNECTION" \
  --network "$NETWORK" \
  --root-disk-size "$ROOT_DISK_SIZE"

incus image export "$CANDIDATE_ALIAS" \
  "$OUTPUT_DIR/dorf-codex-x86_64" --vm

CODEX_VERSION="$(
  sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$METADATA_PATH"
)"
if [[ -z "$CODEX_VERSION" ]]; then
  echo "Candidate metadata did not contain a Codex version." >&2
  exit 1
fi
RELEASE_TAG="${RELEASE_TAG:-room-image-$(date -u +%Y%m%dT%H%M%SZ)-$CODEX_VERSION}"
VALIDATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

uv run --project "$PROJECT_ROOT" python \
  "$SCRIPT_DIR/create-dorf-codex-manifest.py" \
  --archive "$ARCHIVE_PATH" \
  --image-metadata "$METADATA_PATH" \
  --release-tag "$RELEASE_TAG" \
  --source-commit "$SOURCE_COMMIT" \
  --validated-at "$VALIDATED_AT" \
  --output "$OUTPUT_DIR/dorf-codex-x86_64.json"

printf '%s\n' \
  "Candidate ready: $RELEASE_TAG" \
  "Archive: $ARCHIVE_PATH" \
  "Manifest: $OUTPUT_DIR/dorf-codex-x86_64.json"
