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
MANIFEST_PATH="$OUTPUT_DIR/dorf-codex-x86_64.json"
ARCHIVE_PATH="$OUTPUT_DIR/dorf-codex-x86_64.tar.gz"

for command in gh incus jq npm uv; do
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
  echo "Official Room images require a public GitHub repository." >&2
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

echo "Building and validating the official Room image locally."
echo "The Provider Gateway credential remains on this host."
PROVIDER_CONNECTION="$PROVIDER_CONNECTION" \
OUTPUT_DIR="$OUTPUT_DIR" \
SOURCE_COMMIT="$SOURCE_COMMIT" \
  "$SCRIPT_DIR/prepare-dorf-codex-release.sh"

RELEASE_TAG="$(jq -r .release_tag "$MANIFEST_PATH")"
CODEX_VERSION="$(jq -r .codex.version "$MANIFEST_PATH")"
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
  "Credential-free Incus VM image promoted after a real Dorf Codex turn." \
  "" \
  "Codex: $CODEX_VERSION" \
  "Architecture: x86_64" \
  "Source commit: $SOURCE_COMMIT" >"$NOTES_PATH"

echo "Creating complete draft release: $RELEASE_TAG"
gh release create "$RELEASE_TAG" \
  --repo "$GITHUB_REPOSITORY" \
  --draft \
  --latest=false \
  --target "$SOURCE_COMMIT" \
  --title "Dorf Room image · $RELEASE_TAG" \
  --notes-file "$NOTES_PATH" \
  "$ARCHIVE_PATH" \
  "$MANIFEST_PATH"

echo "Publishing and verifying immutable release: $RELEASE_TAG"
gh release edit "$RELEASE_TAG" \
  --repo "$GITHUB_REPOSITORY" \
  --draft=false \
  --latest=false
gh release verify "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY"
gh release verify-asset "$RELEASE_TAG" "$ARCHIVE_PATH" \
  --repo "$GITHUB_REPOSITORY"
gh release verify-asset "$RELEASE_TAG" "$MANIFEST_PATH" \
  --repo "$GITHUB_REPOSITORY"

echo "Published verified official Room image: $RELEASE_TAG"
