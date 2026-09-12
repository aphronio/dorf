#!/usr/bin/env bash
set -euo pipefail

readonly BASE_FINGERPRINT="${BASE_FINGERPRINT:?Set BASE_FINGERPRINT to one exact 64-character Incus image fingerprint}"
readonly IMAGE_ALIAS="${IMAGE_ALIAS:?Set IMAGE_ALIAS to a non-default alias for the opt-in artifact}"
readonly BUILD_VM="${BUILD_VM:-dorf-here-now-build}"
readonly NETWORK="${NETWORK:-incusbr0}"
readonly ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
readonly IMAGE_METADATA_PATH="${IMAGE_METADATA_PATH:-}"
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly PROVISIONER="$SCRIPT_DIR/../sandbox/provision-here-now.sh"
readonly ASSETS="$SCRIPT_DIR/../sandbox/here-now"

if [[ ! "$BASE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "BASE_FINGERPRINT must be one exact lowercase Incus image fingerprint." >&2
  exit 1
fi
if [[ "$IMAGE_ALIAS" == "dorf" ]]; then
  echo "The here.now capability is opt-in and cannot replace the official dorf alias." >&2
  exit 1
fi
for path in "$PROVISIONER" "$ASSETS/SKILL.md" "$ASSETS/publish.sh"; do
  if [[ ! -f "$path" || -L "$path" ]]; then
    echo "Required here.now provisioning input is unavailable: $path" >&2
    exit 1
  fi
done
SOURCE_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse HEAD)"
if [[ ! "$SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] ||
  [[ -n "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=all)" ]]; then
  echo "The opt-in image requires one clean exact Dorf source commit." >&2
  exit 1
fi

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
incus image info "$BASE_FINGERPRINT" >/dev/null
incus init "$BASE_FINGERPRINT" "$BUILD_VM" \
  --vm --network "$NETWORK" -d "root,size=$ROOT_DISK_SIZE"
incus start "$BUILD_VM"
for _ in {1..60}; do
  if incus exec "$BUILD_VM" -- true >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
incus exec "$BUILD_VM" -- true >/dev/null

incus exec "$BUILD_VM" -- install -d -m 0700 /tmp/dorf-here-now-assets
incus file push "$ASSETS/SKILL.md" "$BUILD_VM/tmp/dorf-here-now-assets/SKILL.md"
incus file push "$ASSETS/publish.sh" "$BUILD_VM/tmp/dorf-here-now-assets/publish.sh"
incus file push "$PROVISIONER" "$BUILD_VM/tmp/provision-here-now.sh"
incus exec "$BUILD_VM" -- env \
  DORF_HERE_NOW_ASSETS_DIR=/tmp/dorf-here-now-assets \
  DORF_HERE_NOW_DORF_COMMIT="$SOURCE_COMMIT" \
  DORF_HERE_NOW_PARENT_IMAGE_FINGERPRINT="$BASE_FINGERPRINT" \
  bash /tmp/provision-here-now.sh
incus exec "$BUILD_VM" -- rm -rf -- /tmp/dorf-here-now-assets /tmp/provision-here-now.sh

CODEX_VERSION="$(incus exec "$BUILD_VM" -- jq -er .harnesses.codex.version /usr/local/share/dorf/image.json)"
PI_VERSION="$(incus exec "$BUILD_VM" -- jq -er .harnesses.pi.version /usr/local/share/dorf/image.json)"
incus exec "$BUILD_VM" -- jq -e \
  '.capabilities["here-now"].version == "1.29.0" and
   .capabilities["here-now"].harness == "codex"' \
  /usr/local/share/dorf/image.json >/dev/null
if [[ -n "$IMAGE_METADATA_PATH" ]]; then
  mkdir -p "$(dirname -- "$IMAGE_METADATA_PATH")"
  incus file pull "$BUILD_VM/usr/local/share/dorf/image.json" "$IMAGE_METADATA_PATH"
fi

incus exec "$BUILD_VM" -- sync
incus stop "$BUILD_VM" --timeout 60
incus publish "$BUILD_VM" --alias "$IMAGE_ALIAS" --reuse \
  description="Dorf Codex $CODEX_VERSION and Pi $PI_VERSION with opt-in here.now 1.29.0 publishing" \
  "dorf.codex.version=$CODEX_VERSION" \
  "dorf.pi.version=$PI_VERSION" \
  "dorf.capabilities=here-now@1.29.0" \
  "dorf.capability.base_fingerprint=$BASE_FINGERPRINT" \
  "dorf.capability.source_commit=$SOURCE_COMMIT"

echo "Published opt-in here.now Incus image alias: $IMAGE_ALIAS"
