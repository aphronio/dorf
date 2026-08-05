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
VALIDATION_VM="dorf-coder-workstation-proof-$BUILD_ID"
METADATA_PATH="$OUTPUT_DIR/image.json"
ARCHIVE_BASENAME="dorf-codex-incus-vm-v3-x86_64"
ARCHIVE_PATH="$OUTPUT_DIR/$ARCHIVE_BASENAME.tar.gz"
EVIDENCE_DIR="$OUTPUT_DIR/workstation-evidence"
EVIDENCE_POLICY="${EVIDENCE_POLICY:-retain}"

if [[ "$EVIDENCE_POLICY" != "retain" && "$EVIDENCE_POLICY" != "remove" ]]; then
  echo "EVIDENCE_POLICY must be retain or remove." >&2
  exit 2
fi

cleanup() {
  if [[ "$EVIDENCE_POLICY" == "remove" ]]; then
    rm -rf -- "$EVIDENCE_DIR"
  fi
  if incus info "$VALIDATION_VM" >/dev/null 2>&1; then
    incus delete "$VALIDATION_VM" --force >/dev/null 2>&1 || true
  fi
  if incus info "$PROBE_VM" >/dev/null 2>&1; then
    incus delete "$PROBE_VM" --force >/dev/null 2>&1 || true
  fi
  if incus image info "$CANDIDATE_ALIAS" >/dev/null 2>&1; then
    incus image delete "$CANDIDATE_ALIAS" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

uv run --project "$PROJECT_ROOT" python \
  "$SCRIPT_DIR/validate-dorf-codex-image.py" \
  --provider-connection "$PROVIDER_CONNECTION" \
  --network "$NETWORK" \
  --preflight-only

mkdir -p "$OUTPUT_DIR"
rm -f "$METADATA_PATH" "$ARCHIVE_PATH" \
  "$OUTPUT_DIR/$ARCHIVE_BASENAME.json"

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
incus file push \
  "$SCRIPT_DIR/assert-dorf-codex-credential-free.sh" \
  "$PROBE_VM/tmp/assert-dorf-codex-credential-free.sh"
incus exec "$PROBE_VM" -- chmod +x /tmp/assert-dorf-codex-credential-free.sh
incus exec "$PROBE_VM" -- /tmp/assert-dorf-codex-credential-free.sh
incus exec "$PROBE_VM" -- rm -f /tmp/assert-dorf-codex-credential-free.sh
incus exec "$PROBE_VM" -- bash -lc '
  test -x "$(command -v codex)"
  test -x "$(command -v git)"
  test -x "$(command -v node)"
  ! command -v npm >/dev/null
  test -x "$(command -v uv)"
  test -r /usr/local/share/dorf/image.json
'
incus delete "$PROBE_VM" --force

CANDIDATE_FINGERPRINT="$(
  incus image info "$CANDIDATE_ALIAS" |
    sed -n 's/^Fingerprint: //p'
)"
if [[ ! "$CANDIDATE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Could not resolve candidate image fingerprint." >&2
  exit 1
fi
rm -rf -- "$EVIDENCE_DIR"
uv run --project "$PROJECT_ROOT" python \
  "$SCRIPT_DIR/validate-dorf-coding-workstation.py" \
  --image "$CANDIDATE_ALIAS" \
  --image-fingerprint "$CANDIDATE_FINGERPRINT" \
  --provider-connection "$PROVIDER_CONNECTION" \
  --source-commit "$SOURCE_COMMIT" \
  --proof-id "$BUILD_ID" \
  --project-root "$PROJECT_ROOT" \
  --evidence-dir "$EVIDENCE_DIR" \
  --network "$NETWORK" \
  --root-disk-size "$ROOT_DISK_SIZE"

if [[ "$EVIDENCE_POLICY" == "remove" ]]; then
  rm -rf -- "$EVIDENCE_DIR"
  echo "Temporary workstation evidence removed by policy."
else
  echo "Workstation evidence retained: $EVIDENCE_DIR"
fi

incus image export "$CANDIDATE_ALIAS" \
  "$OUTPUT_DIR/$ARCHIVE_BASENAME" --vm

CODEX_VERSION="$(
  sed -n 's/.*"version": "\([^"]*\)".*/\1/p' "$METADATA_PATH"
)"
if [[ -z "$CODEX_VERSION" ]]; then
  echo "Candidate metadata did not contain a Codex version." >&2
  exit 1
fi
PACKAGE_VERSION="$(
  uv run --project "$PROJECT_ROOT" python -c \
    'import tomllib; print(tomllib.load(open("pyproject.toml", "rb"))["project"]["version"])'
)"
RELEASE_TAG="${RELEASE_TAG:-v$PACKAGE_VERSION}"
VALIDATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

uv run --project "$PROJECT_ROOT" python \
  "$SCRIPT_DIR/create-dorf-codex-manifest.py" \
  --archive "$ARCHIVE_PATH" \
  --image-metadata "$METADATA_PATH" \
  --release-tag "$RELEASE_TAG" \
  --source-commit "$SOURCE_COMMIT" \
  --validated-at "$VALIDATED_AT" \
  --output "$OUTPUT_DIR/$ARCHIVE_BASENAME.json"

printf '%s\n' \
  "Candidate ready: $RELEASE_TAG" \
  "Archive: $ARCHIVE_PATH" \
  "Manifest: $OUTPUT_DIR/$ARCHIVE_BASENAME.json"
