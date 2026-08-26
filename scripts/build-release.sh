#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly MISE="$PROJECT_ROOT/.dorf/bin/mise"
readonly OUTPUT_DIR="${1:-$PROJECT_ROOT/dist/release}"
readonly DOCKERFILE="$PROJECT_ROOT/internal/release/container/Dockerfile"
readonly DOCKERIGNORE="$PROJECT_ROOT/internal/release/container/.dockerignore"
readonly DOCKER_HELPER="$PROJECT_ROOT/scripts/bootstrap/docker.sh"
readonly INCUS_HELPER="$PROJECT_ROOT/scripts/bootstrap/incus.sh"
readonly CONTAINER_REPOSITORY="ghcr.io/aphronio/dorf"
readonly -a CONTAINER_PROOF_ARGS=(
  --rm
  --pull never
  --network none
  --read-only
  --cap-drop ALL
  --security-opt no-new-privileges
  --user 65534:65534
)

loaded_image_ref=""
loaded_image_id=""

cleanup_loaded_image() {
  local current_id

  [[ -n "$loaded_image_ref" ]] || return 0
  current_id="$(docker image inspect --format '{{.Id}}' "$loaded_image_ref" 2>/dev/null || true)"
  if [[ -z "$current_id" ]]; then
    loaded_image_ref=""
    loaded_image_id=""
    return 0
  fi
  if [[ -z "$loaded_image_id" || "$current_id" != "$loaded_image_id" ]]; then
    echo "Refusing to remove changed Docker image reference: $loaded_image_ref" >&2
    return 1
  fi
  docker image rm "$loaded_image_ref" >/dev/null
  loaded_image_ref=""
  loaded_image_id=""
}

if [[ ! -x "$MISE" ]]; then
  echo "Repository toolchain is unavailable; run scripts/dev/setup.sh first." >&2
  exit 1
fi
if ! command -v git >/dev/null 2>&1; then
  echo "Required release command is unavailable: git" >&2
  exit 1
fi

source_is_exact_and_clean() {
  [[ "$(git -C "$PROJECT_ROOT" rev-parse HEAD)" == "$SOURCE_COMMIT" ]] &&
    [[ -z "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=all)" ]]
}

verify_binary_provenance() {
  local binary="$1"
  local metadata

  metadata="$("$MISE" -C "$PROJECT_ROOT" exec -- go version -m -json "$binary")"
  jq -e --arg source "$SOURCE_COMMIT" '
    def setting($key): [.Settings[] | select(.Key == $key) | .Value];
    setting("vcs") == ["git"] and
    setting("vcs.revision") == [$source] and
    setting("vcs.modified") == ["false"] and
    setting("GOOS") == ["linux"] and
    setting("GOARCH") == ["amd64"] and
    setting("CGO_ENABLED") == ["0"]
  ' <<<"$metadata" >/dev/null
}

verify_container_archive() {
  local archive="$1"
  local binary_sha256="$2"
  local config_digest config_path container_binary_sha256 expected_image_id
  local image_metadata manifest observed_image_id

  manifest="$(tar -xOf "$archive" manifest.json)"
  if ! jq -e --arg ref "$CONTAINER_IMAGE" \
    'type == "array" and length == 1 and .[0].RepoTags == [$ref]' \
    <<<"$manifest" >/dev/null; then
    echo "Container archive does not contain the one exact release image reference." >&2
    return 1
  fi
  config_path="$(jq -er '.[0].Config' <<<"$manifest")"
  config_digest="${config_path##*/}"
  config_digest="${config_digest%.json}"
  [[ "$config_digest" =~ ^[0-9a-f]{64}$ ]] || {
    echo "Container archive does not identify one exact image configuration digest." >&2
    return 1
  }
  if docker image inspect "$CONTAINER_IMAGE" >/dev/null 2>&1; then
    echo "Refusing to replace an existing Docker image reference: $CONTAINER_IMAGE" >&2
    return 1
  fi
  expected_image_id="sha256:$config_digest"
  loaded_image_ref="$CONTAINER_IMAGE"
  loaded_image_id="$expected_image_id"
  docker image load --input "$archive" >/dev/null
  observed_image_id="$(docker image inspect --format '{{.Id}}' "$loaded_image_ref")"
  [[ "$observed_image_id" == "$loaded_image_id" ]] || {
    echo "Loaded release image ID does not match its archived configuration digest." >&2
    return 1
  }
  image_metadata="$(docker image inspect "$loaded_image_id")"
  if ! jq -e \
    --arg id "$loaded_image_id" \
    --arg ref "$CONTAINER_IMAGE" \
    --arg release "$VERSION" \
    --arg binary "$binary_sha256" '
      type == "array" and length == 1 and
      .[0].Id == $id and .[0].RepoTags == [$ref] and
      .[0].Os == "linux" and .[0].Architecture == "amd64" and
      .[0].Config.Labels["org.opencontainers.image.version"] == $release and
      .[0].Config.Labels["dev.dorf.binary-sha256"] == $binary
    ' <<<"$image_metadata" >/dev/null; then
    echo "Loaded release image identity or labels do not match the application artifact." >&2
    return 1
  fi
  [[ "$(docker run "${CONTAINER_PROOF_ARGS[@]}" \
    "$loaded_image_id" version)" == "dorf $VERSION" ]] || {
    echo "Loaded release image reports the wrong product version." >&2
    return 1
  }
  container_binary_sha256="$(docker run "${CONTAINER_PROOF_ARGS[@]}" \
    --entrypoint /usr/bin/sha256sum \
    "$loaded_image_id" /usr/local/bin/dorf | awk '{print $1}')"
  [[ "$container_binary_sha256" == "$binary_sha256" ]] || {
    echo "Loaded release image does not contain the exact application binary." >&2
    return 1
  }
  cleanup_loaded_image
}

readonly SOURCE_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse HEAD)"
if ! source_is_exact_and_clean; then
  echo "Release artifacts require the exact clean source commit." >&2
  exit 1
fi

readonly VERSION="$("$MISE" -C "$PROJECT_ROOT" exec -- go run ./cmd/dorf version | awk '{print $2}')"
readonly ARTIFACT_BASENAME="dorf_${VERSION}_linux_x86_64"
readonly ARCHIVE="${ARTIFACT_BASENAME}.tar.gz"
readonly CONTAINER_ARCHIVE="${ARTIFACT_BASENAME}_container-image.docker.tar"
readonly CONTAINER_IMAGE="${CONTAINER_REPOSITORY}:${VERSION}"
readonly INSTALLER="$OUTPUT_DIR/install.sh"
readonly STAGE="$(mktemp -d)"
cleanup() {
  cleanup_loaded_image >/dev/null 2>&1 || true
  rm -rf -- "$STAGE"
}
trap cleanup EXIT

for command in docker jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required release command is unavailable: $command" >&2
    exit 1
  fi
done
if ! docker buildx version >/dev/null 2>&1; then
  echo "Docker Buildx is required to build the release container image archive." >&2
  exit 1
fi
if ! docker buildx inspect >/dev/null 2>&1; then
  echo "A working Docker Buildx builder is required to build the release container image archive." >&2
  exit 1
fi
if [[ ! -f "$DOCKERFILE" ]]; then
  echo "Canonical release container recipe is unavailable: $DOCKERFILE" >&2
  exit 1
fi
if [[ ! -f "$DOCKERIGNORE" ]]; then
  echo "Canonical release container context filter is unavailable: $DOCKERIGNORE" >&2
  exit 1
fi
for helper in "$DOCKER_HELPER" "$INCUS_HELPER"; do
  if [[ ! -f "$helper" ]]; then
    echo "Canonical administrator helper is unavailable: $helper" >&2
    exit 1
  fi
done

mkdir -p "$OUTPUT_DIR" "$STAGE/context/bootstrap" "$STAGE/artifacts"
rm -f -- \
  "$OUTPUT_DIR/$ARCHIVE" \
  "$OUTPUT_DIR/$CONTAINER_ARCHIVE" \
  "$OUTPUT_DIR/dorf_${VERSION}_checksums.txt"
sed "s/@DORF_VERSION@/v$VERSION/g" "$PROJECT_ROOT/scripts/install.sh" >"$INSTALLER"
chmod 0755 "$INSTALLER"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  "$MISE" -C "$PROJECT_ROOT" exec -- go build -trimpath -ldflags='-s -w' \
  -o "$STAGE/context/dorf" ./cmd/dorf
if ! verify_binary_provenance "$STAGE/context/dorf"; then
  echo "Release binary does not prove exact clean source commit $SOURCE_COMMIT." >&2
  exit 1
fi
install -m 0644 "$PROJECT_ROOT/LICENSE" "$STAGE/context/LICENSE"
install -m 0644 "$DOCKERIGNORE" "$STAGE/context/.dockerignore"
install -m 0755 "$DOCKER_HELPER" "$STAGE/context/bootstrap/docker.sh"
install -m 0755 "$INCUS_HELPER" "$STAGE/context/bootstrap/incus.sh"
binary_sha256="$(sha256sum "$STAGE/context/dorf" | awk '{print $1}')"

tar -C "$STAGE/context" -czf "$STAGE/artifacts/$ARCHIVE" \
  dorf LICENSE bootstrap/docker.sh bootstrap/incus.sh
docker buildx build \
  --platform linux/amd64 \
  --pull \
  --provenance=false \
  --file "$DOCKERFILE" \
  --tag "$CONTAINER_IMAGE" \
  --build-arg "DORF_RELEASE=$VERSION" \
  --build-arg "DORF_BINARY_SHA256=$binary_sha256" \
  --output "type=docker,dest=$STAGE/artifacts/$CONTAINER_ARCHIVE" \
  "$STAGE/context"
verify_container_archive "$STAGE/artifacts/$CONTAINER_ARCHIVE" "$binary_sha256"
install -m 0644 "$STAGE/artifacts/$ARCHIVE" "$OUTPUT_DIR/$ARCHIVE"
install -m 0644 "$STAGE/artifacts/$CONTAINER_ARCHIVE" "$OUTPUT_DIR/$CONTAINER_ARCHIVE"
(
  cd "$OUTPUT_DIR"
  sha256sum "$ARCHIVE" "$CONTAINER_ARCHIVE" >"dorf_${VERSION}_checksums.txt"
)
if ! source_is_exact_and_clean; then
  echo "Source changed while release artifacts were being built." >&2
  exit 1
fi
printf '%s\n' \
  "Go release ready: $OUTPUT_DIR/$ARCHIVE" \
  "Container image archive ready: $OUTPUT_DIR/$CONTAINER_ARCHIVE"
