#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo "usage: $0" >&2
  exit 2
fi
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly OUTPUT_DIR="${OUTPUT_DIR:-$PROJECT_ROOT/dist/incus-image}"
readonly CANDIDATE_NETWORK="${NETWORK:-incusbr0}"
readonly CANDIDATE_ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
readonly SOURCE_COMMIT="${SOURCE_COMMIT:-$(git -C "$PROJECT_ROOT" rev-parse HEAD)}"
readonly BUILD_ID="$(date -u +%Y%m%d%H%M%S)"
readonly CANDIDATE_ALIAS="dorf-candidate-$BUILD_ID"
readonly CANDIDATE_BUILD_VM="dorf-build-$BUILD_ID"
readonly ARCHIVE_BASENAME="dorf-incus-vm-v5-x86_64"
readonly ARCHIVE_PATH="$OUTPUT_DIR/$ARCHIVE_BASENAME.tar.gz"
readonly MANIFEST_PATH="$OUTPUT_DIR/$ARCHIVE_BASENAME.json"
readonly METADATA_PATH="$OUTPUT_DIR/image.json"
readonly BUILD_ROOT="$(mktemp -d)"
readonly BINARY="$BUILD_ROOT/dorf"

cleanup() {
  if command -v incus >/dev/null 2>&1 && incus image info "$CANDIDATE_ALIAS" >/dev/null 2>&1; then
    incus image delete "$CANDIDATE_ALIAS" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$BUILD_ROOT"
}
trap cleanup EXIT

if [[ "$(git -C "$PROJECT_ROOT" rev-parse HEAD)" != "$SOURCE_COMMIT" ]] ||
  [[ -n "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=all)" ]]; then
  echo "Release validation requires the exact clean source commit." >&2
  exit 1
fi

for command in go incus git jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required release command is unavailable: $command" >&2
    exit 1
  fi
done

"$SCRIPT_DIR/check-image-inputs.sh"
PRODUCT_VERSION="$(go -C "$PROJECT_ROOT" run ./cmd/dorf version | awk '{print $2}')"
RELEASE_TAG="${RELEASE_TAG:-v$PRODUCT_VERSION}"
OFFICIAL_IMAGE_RELEASE="$(jq -er .release_tag "$PROJECT_ROOT/internal/release/official_image.json")"
if [[ "$RELEASE_TAG" != "v$PRODUCT_VERSION" ]] || [[ "$OFFICIAL_IMAGE_RELEASE" != "$RELEASE_TAG" ]]; then
  echo "Incus image promotion requires the application release and official image pin to agree: application v$PRODUCT_VERSION, image $OFFICIAL_IMAGE_RELEASE, requested $RELEASE_TAG." >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"
rm -f "$METADATA_PATH" "$ARCHIVE_PATH" "$MANIFEST_PATH"
go -C "$PROJECT_ROOT" build -o "$BINARY" ./cmd/dorf

IMAGE_ALIAS="$CANDIDATE_ALIAS" \
BUILD_VM="$CANDIDATE_BUILD_VM" \
NETWORK="$CANDIDATE_NETWORK" \
ROOT_DISK_SIZE="$CANDIDATE_ROOT_DISK_SIZE" \
IMAGE_METADATA_PATH="$METADATA_PATH" \
  "$SCRIPT_DIR/build-dorf-image.sh"

CANDIDATE_FINGERPRINT="$(incus image info "$CANDIDATE_ALIAS" | sed -n 's/^Fingerprint: //p')"
if [[ ! "$CANDIDATE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Could not resolve candidate image fingerprint." >&2
  exit 1
fi

incus image export "$CANDIDATE_ALIAS" "$OUTPUT_DIR/$ARCHIVE_BASENAME" --vm
"$BINARY" release-manifest \
  --archive "$ARCHIVE_PATH" \
  --image-metadata "$METADATA_PATH" \
  --release-tag "$RELEASE_TAG" \
  --source-commit "$SOURCE_COMMIT" \
  --validated-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --output "$MANIFEST_PATH"

printf '%s\n' \
  "Incus image candidate ready: $RELEASE_TAG" \
  "Archive: $ARCHIVE_PATH" \
  "Manifest: $MANIFEST_PATH"
