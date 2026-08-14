#!/usr/bin/env bash
set -euo pipefail

readonly IMAGE_ALIAS="${IMAGE_ALIAS:-dorf}"
readonly BUILD_VM="${BUILD_VM:-dorf-build}"
readonly BASE_IMAGE="images:debian/13"
readonly NETWORK="${NETWORK:-incusbr0}"
readonly ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
readonly IMAGE_METADATA_PATH="${IMAGE_METADATA_PATH:-}"
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly GUEST_SCRIPT="$SCRIPT_DIR/../sandbox/provision-dorf-guest.sh"

cleanup() {
  if incus info "$BUILD_VM" >/dev/null 2>&1; then
    incus delete "$BUILD_VM" --force >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if incus info "$BUILD_VM" >/dev/null 2>&1; then
  echo "Build VM already exists: $BUILD_VM" >&2
  exit 1
fi

BASE_FINGERPRINT="$(incus image info "$BASE_IMAGE" --vm | sed -n 's/^Fingerprint: //p')"
if [[ ! "$BASE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Could not resolve an immutable VM fingerprint for $BASE_IMAGE" >&2
  exit 1
fi

incus init "images:$BASE_FINGERPRINT" "$BUILD_VM" \
  --vm --network "$NETWORK" -d "root,size=$ROOT_DISK_SIZE"
incus start "$BUILD_VM"
for _ in {1..60}; do
  if incus exec "$BUILD_VM" -- true >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
incus exec "$BUILD_VM" -- true >/dev/null
incus file push "$GUEST_SCRIPT" "$BUILD_VM/tmp/provision-dorf-guest.sh"
incus exec "$BUILD_VM" -- chmod +x /tmp/provision-dorf-guest.sh
incus exec "$BUILD_VM" -- env \
  "DORF_BASE_IMAGE=$BASE_IMAGE" \
  "DORF_BASE_FINGERPRINT=$BASE_FINGERPRINT" \
  /tmp/provision-dorf-guest.sh
incus exec "$BUILD_VM" -- rm -f /tmp/provision-dorf-guest.sh

CODEX_VERSION="$(incus exec "$BUILD_VM" -- jq -r .harnesses.codex.version /usr/local/share/dorf/image.json)"
PI_VERSION="$(incus exec "$BUILD_VM" -- jq -r .harnesses.pi.version /usr/local/share/dorf/image.json)"
if [[ -n "$IMAGE_METADATA_PATH" ]]; then
  mkdir -p "$(dirname -- "$IMAGE_METADATA_PATH")"
  incus file pull "$BUILD_VM/usr/local/share/dorf/image.json" "$IMAGE_METADATA_PATH"
fi
incus exec "$BUILD_VM" -- sync
incus stop "$BUILD_VM" --timeout 60
incus publish "$BUILD_VM" --alias "$IMAGE_ALIAS" --reuse \
  description="Dorf Debian 13 VM with Codex $CODEX_VERSION and Pi $PI_VERSION" \
  "dorf.codex.version=$CODEX_VERSION" \
  "dorf.pi.version=$PI_VERSION" \
  dorf.source.base_fingerprint="$BASE_FINGERPRINT"

echo "Published local Incus image alias: $IMAGE_ALIAS"
