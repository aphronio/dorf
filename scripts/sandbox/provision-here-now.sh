#!/usr/bin/env bash
set -euo pipefail

readonly HERE_NOW_VERSION="1.29.0"
readonly HERE_NOW_COMMIT="8cf033ed53b82c0c67b16359c8c431f99e111d04"
readonly UPSTREAM_PUBLISH_SHA256="c5d2341e71ad76fd2dc67009f98799075f92c194082471f9976cafb4a3ab5e83"
readonly UPSTREAM_URL="https://raw.githubusercontent.com/heredotnow/skill/$HERE_NOW_COMMIT/here-now/scripts/publish.sh"
readonly ASSETS_DIR="${DORF_HERE_NOW_ASSETS_DIR:?DORF_HERE_NOW_ASSETS_DIR is required}"
readonly SKILL_DIR="${DORF_HERE_NOW_SKILL_DIR:-/root/.codex/skills/here-now}"
readonly METADATA="${DORF_IMAGE_METADATA_PATH:-/usr/local/share/dorf/image.json}"
readonly CREDENTIALS_FILE="${DORF_HERE_NOW_CREDENTIALS_FILE:-/root/.herenow/credentials}"
readonly DORF_SOURCE_COMMIT="${DORF_HERE_NOW_DORF_COMMIT:?DORF_HERE_NOW_DORF_COMMIT is required}"
readonly PARENT_IMAGE_FINGERPRINT="${DORF_HERE_NOW_PARENT_IMAGE_FINGERPRINT:?DORF_HERE_NOW_PARENT_IMAGE_FINGERPRINT is required}"

die() {
  printf 'error: %s\n' "$1" >&2
  exit 1
}

for command in curl file install jq mktemp python sha256sum; do
  command -v "$command" >/dev/null 2>&1 || die "here.now provisioning requires $command"
done
for path in "$ASSETS_DIR/SKILL.md" "$ASSETS_DIR/publish.sh"; do
  [[ -f "$path" && ! -L "$path" ]] || die "missing here.now adaptation asset: $path"
done
[[ -f "$METADATA" && ! -L "$METADATA" ]] || die "Dorf image metadata is unavailable"
[[ "$DORF_SOURCE_COMMIT" =~ ^[0-9a-f]{40}$ ]] || die "Dorf source commit must be one exact Git identity"
[[ "$PARENT_IMAGE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]] || die "parent image fingerprint must be exact"
[[ ! -e "$CREDENTIALS_FILE" && ! -L "$CREDENTIALS_FILE" ]] ||
  die "refusing to build a Sandbox image containing here.now credentials"

umask 077
temporary="$(mktemp -d "${TMPDIR:-/tmp}/dorf-here-now-provision.XXXXXXXX")"
trap 'rm -rf -- "$temporary"' EXIT

curl --proto '=https' --tlsv1.2 --fail --location --silent --show-error \
  --output "$temporary/publish.sh" "$UPSTREAM_URL"
printf '%s  %s\n' "$UPSTREAM_PUBLISH_SHA256" "$temporary/publish.sh" |
  sha256sum --check --strict >/dev/null

# Preserve the exact reviewed helper and make one narrow transport adaptation:
# authenticated curl headers come from a protected file rather than process argv.
python - "$temporary/publish.sh" "$temporary/publish-upstream.sh" <<'PY'
from pathlib import Path
import sys

source = Path(sys.argv[1]).read_text()
replacements = {
    '  AUTH_ARGS=(-H "authorization: Bearer $API_KEY")': '''  [[ -f "${DORF_HERENOW_AUTH_HEADER_FILE:-}" ]] ||
    die "Dorf requires a protected authorization header file"
  AUTH_ARGS=(-H "@${DORF_HERENOW_AUTH_HEADER_FILE}")''',
    'FINALIZE_URL=$(echo "$RESPONSE" | "$JQ_BIN" -r \'.upload.finalizeUrl\')': '''FINALIZE_URL=$(echo "$RESPONSE" | "$JQ_BIN" -r '.upload.finalizeUrl')
[[ "$FINALIZE_URL" == "$BASE_URL/api/v1/publish/$OUT_SLUG/finalize" ]] ||
  die "here.now returned an unexpected finalize URL"''',
    '  upload_url=$(echo "$RESPONSE" | "$JQ_BIN" -r ".upload.uploads[$i].url")': '''  upload_url=$(echo "$RESPONSE" | "$JQ_BIN" -r ".upload.uploads[$i].url")
  [[ "$upload_url" =~ ^https://[A-Za-z0-9.-]+\\.r2\\.cloudflarestorage\\.com/ ]] ||
    die "here.now returned an unexpected upload URL"''',
}
for needle, replacement in replacements.items():
    if source.count(needle) != 1:
        raise SystemExit("pinned here.now helper no longer matches the reviewed transport")
    source = source.replace(needle, replacement)
Path(sys.argv[2]).write_text(source)
PY

install -d -m 0700 "$SKILL_DIR" "$SKILL_DIR/scripts"
install -m 0600 "$ASSETS_DIR/SKILL.md" "$SKILL_DIR/SKILL.md"
install -m 0700 "$ASSETS_DIR/publish.sh" "$SKILL_DIR/scripts/publish.sh"
install -m 0600 "$temporary/publish-upstream.sh" "$SKILL_DIR/scripts/publish-upstream.sh"

skill_sha256="$(sha256sum "$ASSETS_DIR/SKILL.md" | awk '{print $1}')"
adapter_sha256="$(sha256sum "$ASSETS_DIR/publish.sh" | awk '{print $1}')"
patched_sha256="$(sha256sum "$temporary/publish-upstream.sh" | awk '{print $1}')"
jq \
  --arg version "$HERE_NOW_VERSION" \
  --arg commit "$HERE_NOW_COMMIT" \
  --arg source_sha256 "$UPSTREAM_PUBLISH_SHA256" \
  --arg patched_sha256 "$patched_sha256" \
  --arg skill_sha256 "$skill_sha256" \
  --arg adapter_sha256 "$adapter_sha256" \
  --arg dorf_source_commit "$DORF_SOURCE_COMMIT" \
  --arg parent_image_fingerprint "$PARENT_IMAGE_FINGERPRINT" \
  '.capabilities["here-now"] = {
    version: $version,
    upstream_commit: $commit,
    upstream_publish_sha256: ("sha256:" + $source_sha256),
    installed_publish_sha256: ("sha256:" + $patched_sha256),
    skill_sha256: ("sha256:" + $skill_sha256),
    adapter_sha256: ("sha256:" + $adapter_sha256),
    dorf_source_commit: $dorf_source_commit,
    parent_image_fingerprint: $parent_image_fingerprint,
    harness: "codex",
    authentication: "coordinator-provisioned-file"
  }' "$METADATA" >"$temporary/image.json"
install -m 0644 "$temporary/image.json" "$METADATA"

test ! -e "$CREDENTIALS_FILE"
test -f "$SKILL_DIR/SKILL.md"
test -x "$SKILL_DIR/scripts/publish.sh"
test -f "$SKILL_DIR/scripts/publish-upstream.sh"
test ! -x "$SKILL_DIR/scripts/publish-upstream.sh"
