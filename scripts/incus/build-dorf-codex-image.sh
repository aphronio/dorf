#!/usr/bin/env bash
set -euo pipefail

IMAGE_ALIAS="${IMAGE_ALIAS:-dorf-codex}"
BUILD_VM="${BUILD_VM:-dorf-codex-build}"
BASE_IMAGE="${BASE_IMAGE:-images:ubuntu/24.04}"
NETWORK="${NETWORK:-incusbr0}"
ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
IMAGE_METADATA_PATH="${IMAGE_METADATA_PATH:-}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROVISION_SCRIPT="$SCRIPT_DIR/provision-dorf-codex.sh"

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

BASE_FINGERPRINT="$(
  incus image info "$BASE_IMAGE" --vm |
    sed -n 's/^Fingerprint: //p'
)"
if [[ ! "$BASE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Could not resolve an immutable VM fingerprint for $BASE_IMAGE" >&2
  exit 1
fi

if [[ "$BASE_IMAGE" == *:* ]]; then
  BASE_REMOTE="${BASE_IMAGE%%:*}"
else
  BASE_REMOTE="local"
fi
incus init "$BASE_REMOTE:$BASE_FINGERPRINT" "$BUILD_VM" \
  --vm --network "$NETWORK" -d "root,size=$ROOT_DISK_SIZE"
incus start "$BUILD_VM"

for _ in {1..60}; do
  if incus exec "$BUILD_VM" -- true >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

incus exec "$BUILD_VM" -- true >/dev/null
incus file push "$PROVISION_SCRIPT" "$BUILD_VM/tmp/provision-dorf-codex.sh"
incus exec "$BUILD_VM" -- chmod +x /tmp/provision-dorf-codex.sh
incus exec "$BUILD_VM" -- /tmp/provision-dorf-codex.sh
incus exec "$BUILD_VM" -- rm -f /tmp/provision-dorf-codex.sh
incus exec "$BUILD_VM" -- bash -lc '
  test ! -e /root/.codex/auth.json
  test ! -e /root/.codex/config.toml
  test ! -e /root/.config/dorf/provider-route.key
  test -z "${OPENAI_API_KEY:-}"
'
CODEX_VERSION="$(
  incus exec "$BUILD_VM" -- \
    sed -n 's/.*"version": "\([^"]*\)".*/\1/p' \
    /usr/local/share/dorf/image.json
)"
if [[ -z "$CODEX_VERSION" ]]; then
  echo "Provisioned image did not record its Codex version" >&2
  exit 1
fi
if [[ -n "$IMAGE_METADATA_PATH" ]]; then
  mkdir -p "$(dirname -- "$IMAGE_METADATA_PATH")"
  incus file pull \
    "$BUILD_VM/usr/local/share/dorf/image.json" \
    "$IMAGE_METADATA_PATH"
fi
incus exec "$BUILD_VM" -- sync
incus stop "$BUILD_VM" --timeout 60
incus publish "$BUILD_VM" --alias "$IMAGE_ALIAS" --reuse \
  description="Dorf Ubuntu 24.04 VM with the Codex $CODEX_VERSION harness" \
  dorf.codex.version="$CODEX_VERSION" \
  dorf.source.base_fingerprint="$BASE_FINGERPRINT"

echo "Published Incus image alias: $IMAGE_ALIAS"
incus image list "$IMAGE_ALIAS" --format csv -c lfdtus
