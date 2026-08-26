#!/usr/bin/env bash
set -euo pipefail

PUBLISH=false
if [[ "${1:-}" == "--publish" ]]; then
  PUBLISH=true
  shift
fi
if [[ $# -ne 0 ]]; then
  echo "usage: $0 [--publish]" >&2
  exit 2
fi

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
readonly MISE="${DORF_MISE:-mise}"
readonly GITHUB_REPOSITORY="${GITHUB_REPOSITORY:-aphronio/dorf}"
readonly OUTPUT_DIR="${OUTPUT_DIR:-$PROJECT_ROOT/dist/release}"
readonly IMAGE_DESCRIPTOR="$PROJECT_ROOT/internal/release/official_image.json"
notes_path=""

cleanup() {
  [[ -z "$notes_path" ]] || rm -f -- "$notes_path"
}
trap cleanup EXIT

for command in git jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required release command is unavailable: $command" >&2
    exit 1
  fi
done
if ! command -v "$MISE" >/dev/null 2>&1; then
  echo "Release toolchain is unavailable; run mise install --locked go or set DORF_MISE." >&2
  exit 1
fi
readonly SOURCE_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse HEAD)"
source_is_exact_and_clean() {
  [[ "$(git -C "$PROJECT_ROOT" rev-parse HEAD)" == "$SOURCE_COMMIT" ]] &&
    [[ -z "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=all)" ]]
}
if ! source_is_exact_and_clean; then
  echo "Release validation requires the exact clean source commit." >&2
  exit 1
fi

"$MISE" -C "$PROJECT_ROOT" exec -- "$PROJECT_ROOT/scripts/incus/check-image-inputs.sh"
readonly PRODUCT_VERSION="$("$MISE" -C "$PROJECT_ROOT" exec -- \
  go run ./cmd/dorf version | awk '{print $2}')"
readonly RELEASE_TAG="v$PRODUCT_VERSION"
readonly OFFICIAL_IMAGE_RELEASE="$(jq -er .release_tag "$IMAGE_DESCRIPTOR")"
readonly ARTIFACT_BASENAME="dorf_${PRODUCT_VERSION}_linux_x86_64"
readonly APP_ARCHIVE="$OUTPUT_DIR/${ARTIFACT_BASENAME}.tar.gz"
readonly CHECKSUMS="$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_checksums.txt"
readonly INSTALLER="$OUTPUT_DIR/install.sh"
readonly CONTAINER_IMAGE="ghcr.io/aphronio/dorf:$PRODUCT_VERSION"
readonly IMAGE_ARCHIVE="$OUTPUT_DIR/dorf-incus-vm-v5-x86_64.tar.gz"
readonly IMAGE_MANIFEST="$OUTPUT_DIR/dorf-incus-vm-v5-x86_64.json"

if [[ "$PUBLISH" == true ]]; then
  if ! command -v gh >/dev/null 2>&1; then
    echo "Required publication command is unavailable: gh" >&2
    exit 1
  fi
  if [[ "$(gh api "repos/$GITHUB_REPOSITORY" --jq .visibility | tail -n 1)" != "public" ]]; then
    echo "Official Dorf releases require a public GitHub repository." >&2
    exit 1
  fi
  immutable_releases_enabled="${DORF_IMMUTABLE_RELEASES_ENABLED:-}"
  if [[ -z "$immutable_releases_enabled" ]]; then
    immutable_releases_enabled="$(gh variable get DORF_IMMUTABLE_RELEASES_ENABLED \
      --repo "$GITHUB_REPOSITORY" --json value --jq .value 2>/dev/null | tail -n 1 || true)"
  fi
  if [[ "$immutable_releases_enabled" != "true" ]]; then
    echo "Enable GitHub release immutability, then set DORF_IMMUTABLE_RELEASES_ENABLED=true." >&2
    exit 1
  fi
  if ! gh api "repos/$GITHUB_REPOSITORY/commits/$SOURCE_COMMIT" >/dev/null; then
    echo "Source commit is not available from GitHub: $SOURCE_COMMIT" >&2
    exit 1
  fi
  if gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; then
    echo "Release already exists: $RELEASE_TAG" >&2
    exit 1
  fi
  if [[ "$OFFICIAL_IMAGE_RELEASE" != "$RELEASE_TAG" ]]; then
    gh release verify "$OFFICIAL_IMAGE_RELEASE" --repo "$GITHUB_REPOSITORY" >/dev/null
    if [[ "$(gh release view "$OFFICIAL_IMAGE_RELEASE" --repo "$GITHUB_REPOSITORY" \
      --json isDraft,isImmutable,isPrerelease,assets \
      --jq '[.isImmutable, (.isDraft | not), (.isPrerelease | not), ([.assets[].name] | map(select(. == "dorf-incus-vm-v5-x86_64.tar.gz")) | length == 1), ([.assets[].name] | map(select(. == "dorf-incus-vm-v5-x86_64.json")) | length == 1)] | all' | tail -n 1)" != "true" ]]; then
      echo "Pinned official Incus image release is not immutable and complete: $OFFICIAL_IMAGE_RELEASE" >&2
      exit 1
    fi
  fi
fi

mkdir -p "$OUTPUT_DIR"
rm -f \
  "$INSTALLER" \
  "$APP_ARCHIVE" \
  "$CHECKSUMS" \
  "$IMAGE_ARCHIVE" \
  "$IMAGE_MANIFEST"
build_options=()
if [[ "$PUBLISH" == true ]]; then
  build_options+=(--push)
fi
"$PROJECT_ROOT/scripts/build-release.sh" "${build_options[@]}" "$OUTPUT_DIR"

assets=("$INSTALLER" "$APP_ARCHIVE" "$CHECKSUMS")
image_promoted=false
if [[ "$OFFICIAL_IMAGE_RELEASE" == "$RELEASE_TAG" ]]; then
  if [[ -z "${AI_CONNECTION:-}" ]]; then
    echo "Set AI_CONNECTION to one ready AI connection name for Incus image promotion." >&2
    exit 2
  fi
  env OUTPUT_DIR="$OUTPUT_DIR" RELEASE_TAG="$RELEASE_TAG" \
    "$MISE" -C "$PROJECT_ROOT" exec -- "$PROJECT_ROOT/scripts/incus/release-dorf-image.sh"
  assets+=("$IMAGE_ARCHIVE" "$IMAGE_MANIFEST")
  image_promoted=true
fi

(cd "$OUTPUT_DIR" && sha256sum --check --strict "$(basename "$CHECKSUMS")")
if ! source_is_exact_and_clean; then
  echo "Source changed while release artifacts were being prepared." >&2
  exit 1
fi

printf '%s\n' \
  "Application candidate ready: $RELEASE_TAG" \
  "Archive: $APP_ARCHIVE" \
  "Container image: $CONTAINER_IMAGE" \
  "Installer: $INSTALLER" \
  "Official Incus image release: $OFFICIAL_IMAGE_RELEASE"

if [[ "$PUBLISH" != true ]]; then
  exit
fi

verify_release_attestation() {
  local tag="$1"
  local deadline=$((SECONDS + 600))

  until gh release verify "$tag" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      echo "Timed out waiting for GitHub's signed release attestation for $tag." >&2
      return 1
    fi
    sleep 5
  done
  gh release verify "$tag" --repo "$GITHUB_REPOSITORY"
}

notes_path="$(mktemp)"
{
  printf '%s\n' \
    "Dorf $PRODUCT_VERSION" \
    "" \
    "Go x86_64 Linux application with an immutable release installer." \
    "Linux/amd64 container image: $CONTAINER_IMAGE" \
    "Official Incus image release: $OFFICIAL_IMAGE_RELEASE"
  if [[ "$image_promoted" == true ]]; then
    printf '%s\n' \
      "The image was promoted after real Dorf Codex and Pi turns against one fingerprint." \
      "Codex: $(jq -r .harnesses.codex.version "$IMAGE_MANIFEST")" \
      "Pi: $(jq -r .harnesses.pi.version "$IMAGE_MANIFEST")" \
      "Base: $(jq -r .base_image.reference "$IMAGE_MANIFEST")"
  else
    printf '%s\n' "The previously proven image is reused without rebuilding or republishing it."
  fi
  printf '%s\n' \
    "Environment: Linux x86_64" \
    "Source commit: $SOURCE_COMMIT"
} >"$notes_path"

gh release create "$RELEASE_TAG" \
  --repo "$GITHUB_REPOSITORY" \
  --draft \
  --latest=false \
  --generate-notes \
  --target "$SOURCE_COMMIT" \
  --title "Dorf $RELEASE_TAG" \
  --notes-file "$notes_path" \
  "${assets[@]}"
gh release edit "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --draft=false --latest=false
verify_release_attestation "$RELEASE_TAG"
for asset in "${assets[@]}"; do
  gh release verify-asset "$RELEASE_TAG" "$asset" --repo "$GITHUB_REPOSITORY"
done
gh release edit "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --latest
echo "Published verified Dorf release: $RELEASE_TAG"
