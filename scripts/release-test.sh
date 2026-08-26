#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly WORK_DIR="$(mktemp -d)"
readonly FIXTURE_ROOT="$WORK_DIR/project"
readonly SHIM_DIR="$WORK_DIR/bin"
readonly TEST_STATE="$WORK_DIR/state"
readonly EVENTS="$TEST_STATE/events"
readonly SOURCE_COMMIT="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
readonly IMAGE_ID="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
readonly VERSION="1.2.3"
readonly IMAGE_REF="ghcr.io/aphronio/dorf:$VERSION"

cleanup() { rm -rf -- "$WORK_DIR"; }
trap cleanup EXIT

fail() {
  printf 'release test failed: %s\n' "$*" >&2
  exit 1
}

mkdir -p \
  "$FIXTURE_ROOT/.dorf/bin" \
  "$FIXTURE_ROOT/internal/release/container" \
  "$FIXTURE_ROOT/internal/release" \
  "$FIXTURE_ROOT/scripts/bootstrap" \
  "$FIXTURE_ROOT/scripts/incus" \
  "$SHIM_DIR" \
  "$TEST_STATE"
cp "$PROJECT_ROOT/scripts/release.sh" "$FIXTURE_ROOT/scripts/release.sh"
cp "$PROJECT_ROOT/scripts/build-release.sh" "$FIXTURE_ROOT/scripts/build-release.sh"
printf 'license\n' >"$FIXTURE_ROOT/LICENSE"
printf '@DORF_VERSION@\n' >"$FIXTURE_ROOT/scripts/install.sh"
printf '#!/bin/sh\nprintf "docker helper\\n"\n' >"$FIXTURE_ROOT/scripts/bootstrap/docker.sh"
printf '#!/bin/sh\nprintf "incus helper\\n"\n' >"$FIXTURE_ROOT/scripts/bootstrap/incus.sh"
printf 'FROM scratch\n' >"$FIXTURE_ROOT/internal/release/container/Dockerfile"
printf '*\n!dorf\n' >"$FIXTURE_ROOT/internal/release/container/.dockerignore"
chmod 0755 "$FIXTURE_ROOT/scripts/build-release.sh" "$FIXTURE_ROOT/scripts/release.sh"
printf '{"release_tag":"v%s"}\n' "$VERSION" \
  >"$FIXTURE_ROOT/internal/release/official_image.json"

cat >"$FIXTURE_ROOT/.dorf/bin/mise" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
while [[ $# -gt 0 && "$1" != -- ]]; do shift; done
[[ "${1:-}" == -- ]] || exit 2
shift
if [[ "${1:-}" == go && "${2:-}" == -C ]]; then
  shift 3
fi
if [[ "${1:-}" == go && "${2:-}" == run ]]; then
  printf 'dorf %s\n' "$RELEASE_TEST_VERSION"
  exit 0
fi
if [[ "${1:-}" == go && "${2:-}" == build ]]; then
  output=""
  shift 2
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == -o ]]; then
      output="$2"
      break
    fi
    shift
  done
  [[ -n "$output" ]] || exit 2
  printf '#!/bin/sh\n[ "${1:-}" = version ] || exit 2\nprintf "dorf %s\\n"\n' \
    "$RELEASE_TEST_VERSION" >"$output"
  chmod 0755 "$output"
  exit 0
fi
if [[ "${1:-}" == go && "${2:-}" == version && "${3:-}" == -m ]]; then
  [[ "${4:-}" == -json ]] || exit 2
  printf '{"Settings":['
  printf '{"Key":"vcs","Value":"git"},'
  printf '{"Key":"vcs.revision","Value":"%s"},' \
    "${RELEASE_TEST_VCS_REVISION:-$RELEASE_TEST_SOURCE_COMMIT}"
  printf '{"Key":"vcs.modified","Value":"%s"},' "${RELEASE_TEST_VCS_MODIFIED:-false}"
  printf '%s\n' \
    '{"Key":"GOOS","Value":"linux"},' \
    '{"Key":"GOARCH","Value":"amd64"},' \
    '{"Key":"CGO_ENABLED","Value":"0"}]}'
  exit 0
fi
exec "$@"
EOF

cat >"$FIXTURE_ROOT/scripts/incus/check-image-inputs.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
EOF

cat >"$FIXTURE_ROOT/scripts/incus/release-dorf-image.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'incus image\n' >"$OUTPUT_DIR/dorf-incus-vm-v5-x86_64.tar.gz"
printf '%s\n' \
  '{"harnesses":{"codex":{"version":"test"},"pi":{"version":"test"}},"base_image":{"reference":"test"}}' \
  >"$OUTPUT_DIR/dorf-incus-vm-v5-x86_64.json"
EOF

cat >"$SHIM_DIR/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == -C ]]; then shift 2; fi
case "${1:-}" in
  rev-parse)
    printf '%s\n' "$RELEASE_TEST_SOURCE_COMMIT"
    ;;
  status)
    count=0
    [[ ! -f "$RELEASE_TEST_STATE/status-count" ]] || count="$(<"$RELEASE_TEST_STATE/status-count")"
    count=$((count + 1))
    printf '%s\n' "$count" >"$RELEASE_TEST_STATE/status-count"
    if [[ "$count" -ge "${RELEASE_TEST_DIRTY_AT_STATUS:-999}" ]]; then
      printf ' M tracked-file\n'
    fi
    ;;
  *) exit 2 ;;
esac
EOF

cat >"$SHIM_DIR/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
echo "release invoked ambient Go instead of the repository toolchain" >&2
exit 97
EOF

cat >"$SHIM_DIR/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker' >>"$RELEASE_TEST_EVENTS"
printf '\t%s' "$@" >>"$RELEASE_TEST_EVENTS"
printf '\n' >>"$RELEASE_TEST_EVENTS"
if [[ "${1:-}" == buildx && "${2:-}" == build ]]; then
  output=""
  binary_sha256=""
  shift 2
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --output)
        output="${2#type=docker,dest=}"
        shift 2
        ;;
      --build-arg)
        if [[ "$2" == DORF_BINARY_SHA256=* ]]; then
          binary_sha256="${2#DORF_BINARY_SHA256=}"
        fi
        shift 2
        ;;
      *) shift ;;
    esac
  done
  [[ -n "$output" && -n "$binary_sha256" ]] || exit 2
  printf '%s\n' "$binary_sha256" >"$RELEASE_TEST_STATE/binary-sha256"
  stage="$(mktemp -d)"
  trap 'rm -rf -- "$stage"' EXIT
  printf '[{"Config":"blobs/sha256/%s","RepoTags":["%s"],"Layers":[]}]\n' \
    "${RELEASE_TEST_IMAGE_ID#sha256:}" "$RELEASE_TEST_IMAGE_REF" >"$stage/manifest.json"
  tar -C "$stage" -cf "$output" manifest.json
  exit 0
fi
if [[ "${1:-}" == buildx ]]; then
  exit 0
fi
if [[ "${1:-}" == image && "${2:-}" == load ]]; then
  : >"$RELEASE_TEST_STATE/docker-loaded"
  exit 0
fi
if [[ "${1:-}" == image && "${2:-}" == rm ]]; then
  [[ "${3:-}" == "$RELEASE_TEST_IMAGE_REF" ]] || exit 2
  rm -f -- "$RELEASE_TEST_STATE/docker-loaded"
  exit 0
fi
if [[ "${1:-}" == image && "${2:-}" == inspect ]]; then
  [[ -f "$RELEASE_TEST_STATE/docker-loaded" ]] || exit 1
  shift 2
  format=""
  if [[ "${1:-}" == --format ]]; then
    format="$2"
    shift 2
  fi
  case "$format" in
    '')
      printf '[{"Id":"%s","RepoTags":["%s"],"Os":"linux","Architecture":"amd64","Config":{"Labels":{"org.opencontainers.image.version":"%s","dev.dorf.binary-sha256":"%s"}}}]\n' \
        "$RELEASE_TEST_IMAGE_ID" "$RELEASE_TEST_IMAGE_REF" "$RELEASE_TEST_VERSION" \
        "$(<"$RELEASE_TEST_STATE/binary-sha256")"
      ;;
    '{{.Id}}') printf '%s\n' "$RELEASE_TEST_IMAGE_ID" ;;
    '{{range .RepoTags}}{{println .}}{{end}}') printf '%s\n' "$RELEASE_TEST_IMAGE_REF" ;;
    '{{.Os}}') printf 'linux\n' ;;
    '{{.Architecture}}') printf 'amd64\n' ;;
    *org.opencontainers.image.version*) printf '%s\n' "$RELEASE_TEST_VERSION" ;;
    *dev.dorf.binary-sha256*) cat "$RELEASE_TEST_STATE/binary-sha256" ;;
    *) exit 2 ;;
  esac
  exit 0
fi
if [[ "${1:-}" == run ]]; then
  if [[ " $* " == *' --entrypoint /usr/bin/sha256sum '* ]]; then
    printf '%s  /usr/local/bin/dorf\n' "$(<"$RELEASE_TEST_STATE/binary-sha256")"
  else
    printf 'dorf %s\n' "$RELEASE_TEST_VERSION"
  fi
  exit 0
fi
exit 2
EOF

cat >"$SHIM_DIR/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'gh' >>"$RELEASE_TEST_EVENTS"
printf '\t%s' "$@" >>"$RELEASE_TEST_EVENTS"
printf '\n' >>"$RELEASE_TEST_EVENTS"
if [[ "${1:-}" == api ]]; then
  if [[ " $* " == *' --jq .visibility '* ]]; then printf 'public\n'; fi
  exit 0
fi
if [[ "${1:-}" == variable && "${2:-}" == get ]]; then
  printf 'true\n'
  exit 0
fi
if [[ "${1:-}" == release && "${2:-}" == view ]]; then
  exit 1
fi
if [[ "${1:-}" == release && "${2:-}" == verify-asset && \
  "${RELEASE_TEST_FAIL_ASSET_VERIFY:-false}" == true ]]; then
  exit 1
fi
if [[ "${1:-}" == release ]]; then
  exit 0
fi
exit 2
EOF

chmod 0755 \
  "$FIXTURE_ROOT/scripts/bootstrap/docker.sh" \
  "$FIXTURE_ROOT/scripts/bootstrap/incus.sh" \
  "$FIXTURE_ROOT/.dorf/bin/mise" \
  "$FIXTURE_ROOT/scripts/incus/check-image-inputs.sh" \
  "$FIXTURE_ROOT/scripts/incus/release-dorf-image.sh" \
  "$SHIM_DIR/docker" \
  "$SHIM_DIR/gh" \
  "$SHIM_DIR/git" \
  "$SHIM_DIR/go"

reset_case() {
  rm -rf -- "$FIXTURE_ROOT/dist" "$TEST_STATE/docker-loaded"
  rm -f -- "$EVENTS" "$TEST_STATE/binary-sha256" "$TEST_STATE/status-count"
  mkdir -p "$FIXTURE_ROOT/dist/release"
}

run_release() {
  env \
    AI_CONNECTION=test \
    GITHUB_REPOSITORY=aphronio/dorf \
    OUTPUT_DIR="$FIXTURE_ROOT/dist/release" \
    PATH="$SHIM_DIR:$PATH" \
    RELEASE_TEST_EVENTS="$EVENTS" \
    RELEASE_TEST_IMAGE_ID="$IMAGE_ID" \
    RELEASE_TEST_IMAGE_REF="$IMAGE_REF" \
    RELEASE_TEST_SOURCE_COMMIT="$SOURCE_COMMIT" \
    RELEASE_TEST_STATE="$TEST_STATE" \
    RELEASE_TEST_VERSION="$VERSION" \
    "$@" \
    "$FIXTURE_ROOT/scripts/release.sh" --publish
}

event_line() {
  local operation="$1"
  awk -F '\t' -v operation="$operation" '
    $1 == "gh" && $2 == "release" && $3 == operation { print NR; exit }
  ' "$EVENTS"
}

last_event_line() {
  local operation="$1"
  awk -F '\t' -v operation="$operation" '
    $1 == "gh" && $2 == "release" && $3 == operation { line = NR }
    END { if (line) print line }
  ' "$EVENTS"
}

edit_line_with_flag() {
  local flag="$1"
  awk -F '\t' -v flag="$flag" '
    $1 == "gh" && $2 == "release" && $3 == "edit" {
      for (i = 4; i <= NF; i++) if ($i == flag) { print NR; exit }
    }
  ' "$EVENTS"
}

test_publish_verifies_before_latest_promotion() {
  local archive create publish verify_release first_asset last_asset promote asset_count proof_prefix

  reset_case
  run_release
  create="$(event_line create)"
  publish="$(edit_line_with_flag --draft=false)"
  verify_release="$(last_event_line verify)"
  first_asset="$(event_line verify-asset)"
  last_asset="$(last_event_line verify-asset)"
  promote="$(edit_line_with_flag --latest)"
  [[ -n "$create" && -n "$publish" && -n "$verify_release" && -n "$first_asset" && \
    -n "$last_asset" && -n "$promote" ]] ||
    fail "publication did not exercise create, publish, release verification, assets, and latest"
  ((create < publish && publish < verify_release && verify_release < first_asset && \
    first_asset <= last_asset && last_asset < promote)) ||
    fail "latest promotion did not follow immutable release and every asset verification"
  asset_count="$(awk -F '\t' '$1 == "gh" && $2 == "release" && $3 == "verify-asset" { count += 1 } END { print count + 0 }' "$EVENTS")"
  [[ "$asset_count" -eq 6 ]] || fail "publication did not verify all six fixture assets"
  awk -F '\t' '
    $1 == "gh" && $2 == "release" && $3 == "create" {
      for (i = 4; i <= NF; i++) if ($i == "--latest=false") found = 1
    }
    END { exit(found ? 0 : 1) }
  ' "$EVENTS" || fail "draft release was not explicitly created non-latest"
  awk -F '\t' '
    $1 == "gh" && $2 == "release" && $3 == "edit" {
      draft = latest_false = 0
      for (i = 4; i <= NF; i++) {
        if ($i == "--draft=false") draft = 1
        if ($i == "--latest=false") latest_false = 1
      }
      if (draft && latest_false) found = 1
    }
    END { exit(found ? 0 : 1) }
  ' "$EVENTS" || fail "release was not explicitly published non-latest"
  [[ ! -e "$TEST_STATE/docker-loaded" ]] || fail "container proof left its image reference loaded"
  grep -F $'docker\timage\tload' "$EVENTS" >/dev/null || fail "container archive was not loaded"
  proof_prefix=$'docker\trun\t--rm\t--pull\tnever\t--network\tnone\t--read-only\t--cap-drop\tALL\t--security-opt\tno-new-privileges\t--user\t65534:65534'
  grep -F "$proof_prefix"$'\t'"$IMAGE_ID"$'\tversion' "$EVENTS" >/dev/null ||
    fail "loaded image was not run by exact image ID"
  grep -F "$proof_prefix"$'\t--entrypoint\t/usr/bin/sha256sum\t'"$IMAGE_ID" \
    "$EVENTS" >/dev/null || fail "in-image release binary was not hashed by exact image ID"
  grep -F $'docker\timage\trm\t'"$IMAGE_REF" "$EVENTS" >/dev/null ||
    fail "loaded exact image reference was not cleaned"
  archive="$FIXTURE_ROOT/dist/release/dorf_${VERSION}_linux_x86_64.tar.gz"
  [[ "$(tar -tzf "$archive" | LC_ALL=C sort)" == $'LICENSE\nbootstrap/docker.sh\nbootstrap/incus.sh\ndorf' ]] ||
    fail "application archive does not contain the exact binary, license, and administrator helpers"
  [[ "$(tar -xOf "$archive" bootstrap/docker.sh)" == $'#!/bin/sh\nprintf "docker helper\\n"' ]] ||
    fail "application archive changed the canonical Docker helper"
  [[ "$(tar -xOf "$archive" bootstrap/incus.sh)" == $'#!/bin/sh\nprintf "incus helper\\n"' ]] ||
    fail "application archive changed the canonical Incus helper"
}

test_failed_asset_verification_never_changes_latest() {
  reset_case
  if run_release RELEASE_TEST_FAIL_ASSET_VERIFY=true; then
    fail "release succeeded after asset verification failed"
  fi
  [[ -n "$(edit_line_with_flag --draft=false)" ]] || fail "failure case never published the immutable release"
  [[ -z "$(edit_line_with_flag --latest)" ]] || fail "asset verification failure changed the latest release"
}

test_dirty_or_foreign_binary_never_publishes() {
  reset_case
  if run_release RELEASE_TEST_VCS_MODIFIED=true; then
    fail "release accepted a dirty application binary"
  fi
  [[ -z "$(event_line create)" ]] || fail "dirty application binary reached GitHub publication"

  reset_case
  if run_release RELEASE_TEST_VCS_REVISION=cccccccccccccccccccccccccccccccccccccccc; then
    fail "release accepted an application binary from another commit"
  fi
  [[ -z "$(event_line create)" ]] || fail "foreign application binary reached GitHub publication"

  reset_case
  if run_release RELEASE_TEST_DIRTY_AT_STATUS=3; then
    fail "release accepted source changes made while artifacts were built"
  fi
  [[ -z "$(event_line create)" ]] || fail "post-build source changes reached GitHub publication"
}

test_publish_verifies_before_latest_promotion
test_failed_asset_verification_never_changes_latest
test_dirty_or_foreign_binary_never_publishes

printf 'release tests passed\n'
