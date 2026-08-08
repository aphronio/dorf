#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${PROVIDER_CONNECTION:-}" ]]; then
  echo "Set PROVIDER_CONNECTION to one connected local Provider Gateway name." >&2
  exit 2
fi

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
GITHUB_REPOSITORY="${GITHUB_REPOSITORY:-aphronio/dorf}"
OUTPUT_DIR="${OUTPUT_DIR:-$PROJECT_ROOT/dist/room-image}"
SOURCE_COMMIT="${SOURCE_COMMIT:-$(git -C "$PROJECT_ROOT" rev-parse HEAD)}"
MANIFEST_PATH="$OUTPUT_DIR/dorf-codex-incus-vm-v4-x86_64.json"
ARCHIVE_PATH="$OUTPUT_DIR/dorf-codex-incus-vm-v4-x86_64.tar.gz"

for command in gh go incus jq npm; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required release command is unavailable: $command" >&2
    exit 1
  fi
done

if [[ -n "$(git -C "$PROJECT_ROOT" status --porcelain)" ]]; then
  echo "Refusing to publish from a worktree with uncommitted changes." >&2
  exit 1
fi
if [[ "$(gh api "repos/$GITHUB_REPOSITORY" --jq .visibility)" != "public" ]]; then
  echo "Official Sandbox images require a public GitHub repository." >&2
  exit 1
fi
if [[ "$(gh variable get DORF_IMMUTABLE_RELEASES_ENABLED \
  --repo "$GITHUB_REPOSITORY" --json value --jq .value 2>/dev/null || true)" != "true" ]]; then
  echo "Enable GitHub release immutability, then set DORF_IMMUTABLE_RELEASES_ENABLED=true." >&2
  exit 1
fi
if ! gh api "repos/$GITHUB_REPOSITORY/commits/$SOURCE_COMMIT" >/dev/null; then
  echo "Source commit is not available from GitHub: $SOURCE_COMMIT" >&2
  exit 1
fi

echo "Building and validating the official Sandbox image locally."
echo "The Provider Gateway credential remains on this host."
PROVIDER_CONNECTION="$PROVIDER_CONNECTION" \
OUTPUT_DIR="$OUTPUT_DIR" \
SOURCE_COMMIT="$SOURCE_COMMIT" \
  "$SCRIPT_DIR/prepare-dorf-codex-release.sh"
"$PROJECT_ROOT/scripts/build-release.sh" "$OUTPUT_DIR"

RELEASE_TAG="$(jq -r .release_tag "$MANIFEST_PATH")"
CODEX_VERSION="$(jq -r .codex.version "$MANIFEST_PATH")"
PRODUCT_VERSION="$(go -C "$PROJECT_ROOT" run ./cmd/dorf version | awk '{print $2}')"
if [[ "$RELEASE_TAG" != "v$PRODUCT_VERSION" ]]; then
  echo "Image release tag must match Go product version v$PRODUCT_VERSION: $RELEASE_TAG" >&2
  exit 1
fi
if gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; then
  echo "Release already exists: $RELEASE_TAG" >&2
  exit 1
fi

NOTES_PATH="$(mktemp)"
cleanup_notes() {
  rm -f "$NOTES_PATH"
}
trap cleanup_notes EXIT
printf '%s\n' \
  "Dorf $PRODUCT_VERSION" \
  "" \
  "Go x86_64 Linux application and credential-free Incus VM image." \
  "The image was promoted after a real Dorf Codex turn." \
  "" \
  "Codex: $CODEX_VERSION" \
  "Environment: Incus VM" \
  "Architecture: x86_64" \
  "Source commit: $SOURCE_COMMIT" >"$NOTES_PATH"

echo "Creating complete draft release: $RELEASE_TAG"
gh release create "$RELEASE_TAG" \
  --repo "$GITHUB_REPOSITORY" \
  --draft \
  --generate-notes \
  --target "$SOURCE_COMMIT" \
  --title "Dorf $RELEASE_TAG" \
  --notes-file "$NOTES_PATH" \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_linux_x86_64.tar.gz" \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_checksums.txt" \
  "$ARCHIVE_PATH" \
  "$MANIFEST_PATH"

echo "Publishing and verifying immutable release: $RELEASE_TAG"
gh release edit "$RELEASE_TAG" \
  --repo "$GITHUB_REPOSITORY" \
  --draft=false \
  --latest
gh release verify "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY"
gh release verify-asset "$RELEASE_TAG" "$ARCHIVE_PATH" \
  --repo "$GITHUB_REPOSITORY"
gh release verify-asset "$RELEASE_TAG" "$MANIFEST_PATH" \
  --repo "$GITHUB_REPOSITORY"
gh release verify-asset "$RELEASE_TAG" \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_linux_x86_64.tar.gz" \
  --repo "$GITHUB_REPOSITORY"
gh release verify-asset "$RELEASE_TAG" \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_checksums.txt" \
  --repo "$GITHUB_REPOSITORY"

echo "Published verified official Sandbox image: $RELEASE_TAG"
