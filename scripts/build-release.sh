#!/usr/bin/env bash
set -euo pipefail

PUSH_IMAGE=false
if [[ "${1:-}" == "--push" ]]; then
  PUSH_IMAGE=true
  shift
fi
if [[ $# -gt 1 ]]; then
  echo "usage: $0 [--push] [OUTPUT_DIR]" >&2
  exit 2
fi

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly MISE="${DORF_MISE:-mise}"
readonly OUTPUT_DIR="${1:-$PROJECT_ROOT/dist/release}"
readonly DOCKERFILE="$PROJECT_ROOT/internal/release/container/Dockerfile"
readonly DOCKERIGNORE="$PROJECT_ROOT/internal/release/container/.dockerignore"
readonly DOCKER_HELPER="$PROJECT_ROOT/scripts/bootstrap/docker.sh"
readonly INCUS_HELPER="$PROJECT_ROOT/scripts/bootstrap/incus.sh"
readonly INCUS_REMOTE_HELPER="$PROJECT_ROOT/scripts/bootstrap/incus-remote.sh"
readonly COMPOSE_MANIFEST="$PROJECT_ROOT/deploy/compose.yaml"
readonly INCUS_COMPOSE_MANIFEST="$PROJECT_ROOT/deploy/compose.incus.yaml"
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

if ! command -v "$MISE" >/dev/null 2>&1; then
  echo "Release toolchain is unavailable; run mise install --locked go or set DORF_MISE." >&2
  exit 1
fi
for command in git docker jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required release command is unavailable: $command" >&2
    exit 1
  fi
done
if ! docker buildx version >/dev/null 2>&1; then
  echo "Docker Buildx is required to build the release container image." >&2
  exit 1
fi
if ! docker buildx inspect >/dev/null 2>&1; then
  echo "A working Docker Buildx builder is required to build the release container image." >&2
  exit 1
fi
for required in \
  "$DOCKERFILE" \
  "$DOCKERIGNORE" \
  "$DOCKER_HELPER" \
  "$INCUS_HELPER" \
  "$INCUS_REMOTE_HELPER" \
  "$COMPOSE_MANIFEST" \
  "$INCUS_COMPOSE_MANIFEST"; do
  if [[ ! -f "$required" ]]; then
    echo "Required release input is unavailable: $required" >&2
    exit 1
  fi
done

readonly SOURCE_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse HEAD)"
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

if ! source_is_exact_and_clean; then
  echo "Release artifacts require the exact clean source commit." >&2
  exit 1
fi

readonly VERSION="$("$MISE" -C "$PROJECT_ROOT" exec -- go run ./cmd/dorf version | awk '{print $2}')"
readonly ARTIFACT_BASENAME="dorf_${VERSION}_linux_x86_64"
readonly ARCHIVE="${ARTIFACT_BASENAME}.tar.gz"
readonly CONTAINER_IMAGE="${CONTAINER_REPOSITORY}:${VERSION}"
readonly CHECKSUMS="dorf_${VERSION}_checksums.txt"
readonly INSTALLER="$OUTPUT_DIR/install.sh"
readonly STAGE="$(mktemp -d)"
trap 'rm -rf -- "$STAGE"' EXIT

mkdir -p "$OUTPUT_DIR" "$STAGE/context/bootstrap" "$STAGE/artifacts"
rm -f -- "$OUTPUT_DIR/$ARCHIVE" "$OUTPUT_DIR/$CHECKSUMS"
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
install -m 0755 "$INCUS_REMOTE_HELPER" "$STAGE/context/bootstrap/incus-remote.sh"
install -m 0644 "$COMPOSE_MANIFEST" "$STAGE/context/dorf-compose.yaml"
install -m 0644 "$INCUS_COMPOSE_MANIFEST" "$STAGE/context/dorf-compose-incus.yaml"
readonly binary_sha256="$(sha256sum "$STAGE/context/dorf" | awk '{print $1}')"

tar -C "$STAGE/context" -czf "$STAGE/artifacts/$ARCHIVE" \
  dorf dorf-compose.yaml dorf-compose-incus.yaml \
  LICENSE bootstrap/docker.sh bootstrap/incus.sh bootstrap/incus-remote.sh
install -m 0644 "$STAGE/artifacts/$ARCHIVE" "$OUTPUT_DIR/$ARCHIVE"
(
  cd "$OUTPUT_DIR"
  sha256sum "$ARCHIVE" >"$CHECKSUMS"
)
if ! source_is_exact_and_clean; then
  echo "Source changed while release artifacts were being prepared." >&2
  exit 1
fi

image_output=(--load)
if [[ "$PUSH_IMAGE" == true ]]; then
  image_output=(--push)
fi
docker buildx build \
  --platform linux/amd64 \
  --pull \
  --provenance=false \
  --file "$DOCKERFILE" \
  --tag "$CONTAINER_IMAGE" \
  --build-arg "DORF_RELEASE=$VERSION" \
  --build-arg "DORF_BINARY_SHA256=$binary_sha256" \
  "${image_output[@]}" \
  "$STAGE/context"

if [[ "$PUSH_IMAGE" != true ]]; then
  if [[ "$(docker run "${CONTAINER_PROOF_ARGS[@]}" "$CONTAINER_IMAGE" version)" != "dorf $VERSION" ]]; then
    echo "Release container image reports the wrong product version." >&2
    exit 1
  fi
  container_binary_sha256="$(docker run "${CONTAINER_PROOF_ARGS[@]}" \
    --entrypoint /usr/bin/sha256sum "$CONTAINER_IMAGE" /usr/local/bin/dorf | awk '{print $1}')"
  if [[ "$container_binary_sha256" != "$binary_sha256" ]]; then
    echo "Release container image does not contain the exact application binary." >&2
    exit 1
  fi
fi

if ! source_is_exact_and_clean; then
  echo "Source changed while release artifacts were being built." >&2
  exit 1
fi

printf '%s\n' \
  "Go release ready: $OUTPUT_DIR/$ARCHIVE" \
  "Checksums ready: $OUTPUT_DIR/$CHECKSUMS"
if [[ "$PUSH_IMAGE" == true ]]; then
  echo "Container image pushed: $CONTAINER_IMAGE"
else
  echo "Container image loaded: $CONTAINER_IMAGE"
fi
