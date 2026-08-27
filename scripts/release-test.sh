#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly WORK_DIR="$(mktemp -d)"
readonly FIXTURE_ROOT="$WORK_DIR/project"
readonly SHIM_DIR="$WORK_DIR/bin"
readonly TEST_STATE="$WORK_DIR/state"
readonly EVENTS="$TEST_STATE/events"
readonly SOURCE_COMMIT="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
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
  "$FIXTURE_ROOT/deploy" \
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
printf '#!/bin/sh\nprintf "remote incus helper\\n"\n' >"$FIXTURE_ROOT/scripts/bootstrap/incus-remote.sh"
printf 'services:\n  api:\n    image: release-test\n' >"$FIXTURE_ROOT/deploy/compose.yaml"
printf 'services:\n  worker:\n    environment:\n      INCUS_REMOTE: test\n' \
  >"$FIXTURE_ROOT/deploy/compose.incus.yaml"
printf 'FROM scratch\n' >"$FIXTURE_ROOT/internal/release/container/Dockerfile"
printf '*\n!dorf\n' >"$FIXTURE_ROOT/internal/release/container/.dockerignore"
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
if [[ "${1:-}" == buildx && ( "${2:-}" == version || "${2:-}" == inspect ) ]]; then
  exit 0
fi
if [[ "${1:-}" == buildx && "${2:-}" == build ]]; then
  binary_sha256=""
  load=false
  push=false
  shift 2
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --load)
        load=true
        shift
        ;;
      --push)
        push=true
        shift
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
  [[ -n "$binary_sha256" ]] || exit 2
  printf '%s\n' "$binary_sha256" >"$RELEASE_TEST_STATE/binary-sha256"
  if [[ "$push" == true && "${RELEASE_TEST_FAIL_IMAGE_PUSH:-false}" == true ]]; then
    exit 1
  fi
  if [[ "$load" == true ]]; then
    : >"$RELEASE_TEST_STATE/image-loaded"
  fi
  exit 0
fi
if [[ "${1:-}" == run ]]; then
  [[ -f "$RELEASE_TEST_STATE/image-loaded" ]] || exit 1
  [[ " $* " == *" $RELEASE_TEST_IMAGE_REF "* ]] || exit 2
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
  "$FIXTURE_ROOT/scripts/build-release.sh" \
  "$FIXTURE_ROOT/scripts/release.sh" \
  "$FIXTURE_ROOT/scripts/bootstrap/docker.sh" \
  "$FIXTURE_ROOT/scripts/bootstrap/incus.sh" \
  "$FIXTURE_ROOT/scripts/bootstrap/incus-remote.sh" \
  "$FIXTURE_ROOT/.dorf/bin/mise" \
  "$FIXTURE_ROOT/scripts/incus/check-image-inputs.sh" \
  "$FIXTURE_ROOT/scripts/incus/release-dorf-image.sh" \
  "$SHIM_DIR/docker" \
  "$SHIM_DIR/gh" \
  "$SHIM_DIR/git" \
  "$SHIM_DIR/go"

reset_case() {
  rm -rf -- "$FIXTURE_ROOT/dist"
  rm -f -- \
    "$EVENTS" \
    "$TEST_STATE/binary-sha256" \
    "$TEST_STATE/image-loaded" \
    "$TEST_STATE/status-count"
  mkdir -p "$FIXTURE_ROOT/dist/release"
}

release_env() {
  env \
    AI_CONNECTION=test \
    GITHUB_REPOSITORY=aphronio/dorf \
    OUTPUT_DIR="$FIXTURE_ROOT/dist/release" \
    DORF_MISE="$FIXTURE_ROOT/.dorf/bin/mise" \
    PATH="$SHIM_DIR:$PATH" \
    RELEASE_TEST_EVENTS="$EVENTS" \
    RELEASE_TEST_IMAGE_REF="$IMAGE_REF" \
    RELEASE_TEST_SOURCE_COMMIT="$SOURCE_COMMIT" \
    RELEASE_TEST_STATE="$TEST_STATE" \
    RELEASE_TEST_VERSION="$VERSION" \
    "$@"
}

run_release() {
  release_env "$@" "$FIXTURE_ROOT/scripts/release.sh" --publish
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

assert_registry_push() {
  awk -F '\t' -v ref="$IMAGE_REF" '
    $1 == "docker" && $2 == "buildx" && $3 == "build" {
      push = platform = tag = 0
      for (i = 4; i <= NF; i++) {
        if ($i == "--push") push = 1
        if ($i == "--platform" && $(i + 1) == "linux/amd64") platform = 1
        if ($i == "--tag" && $(i + 1) == ref) tag = 1
        if ($i == "--output") bad_output = 1
      }
      if (push && platform && tag && !bad_output) found = 1
    }
    END { exit(found ? 0 : 1) }
  ' "$EVENTS" || fail "release did not push the exact Linux/amd64 registry image"
}

assert_application_artifacts() {
  local archive="$FIXTURE_ROOT/dist/release/dorf_${VERSION}_linux_x86_64.tar.gz"
  local checksums="$FIXTURE_ROOT/dist/release/dorf_${VERSION}_checksums.txt"

  [[ "$(tar -tzf "$archive" | LC_ALL=C sort)" == $'LICENSE\nbootstrap/docker.sh\nbootstrap/incus-remote.sh\nbootstrap/incus.sh\ndorf\ndorf-compose-incus.yaml\ndorf-compose.yaml' ]] ||
    fail "application archive does not contain the exact installable files and administrator helpers"
  [[ "$(tar -xOf "$archive" bootstrap/docker.sh)" == $'#!/bin/sh\nprintf "docker helper\\n"' ]] ||
    fail "application archive changed the canonical Docker helper"
  [[ "$(tar -xOf "$archive" bootstrap/incus.sh)" == $'#!/bin/sh\nprintf "incus helper\\n"' ]] ||
    fail "application archive changed the canonical Incus helper"
  [[ "$(tar -xOf "$archive" bootstrap/incus-remote.sh)" == $'#!/bin/sh\nprintf "remote incus helper\\n"' ]] ||
    fail "application archive changed the canonical remote Incus helper"
  cmp <(tar -xOf "$archive" dorf-compose.yaml) "$FIXTURE_ROOT/deploy/compose.yaml" ||
    fail "application archive changed the canonical Compose manifest"
  cmp <(tar -xOf "$archive" dorf-compose-incus.yaml) "$FIXTURE_ROOT/deploy/compose.incus.yaml" ||
    fail "application archive changed the canonical Incus Compose override"
  [[ "$(wc -l <"$checksums")" -eq 1 ]] || fail "checksums describe more than the application archive"
  grep -F "  $(basename "$archive")" "$checksums" >/dev/null ||
    fail "checksums omit the application archive"
  (cd "$(dirname "$checksums")" && sha256sum --check --strict "$(basename "$checksums")") >/dev/null ||
    fail "application archive checksum does not verify"
  if find "$FIXTURE_ROOT/dist/release" -name '*container-image*' -print -quit | grep -q .; then
    fail "release still produced a container-image archive"
  fi
}

test_publish_verifies_before_latest_promotion() {
  local create publish verify_release first_asset last_asset promote asset_count

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
    fail "publication omitted immutable-release verification or latest promotion"
  ((create < publish && publish < verify_release && verify_release < first_asset && \
    first_asset <= last_asset && last_asset < promote)) ||
    fail "latest promotion did not follow immutable release and asset verification"
  asset_count="$(awk -F '\t' '$1 == "gh" && $2 == "release" && $3 == "verify-asset" { count += 1 } END { print count + 0 }' "$EVENTS")"
  [[ "$asset_count" -eq 5 ]] || fail "publication did not verify all five release assets"
  assert_registry_push
  assert_application_artifacts
}

test_failed_publication_never_changes_latest() {
  reset_case
  if run_release RELEASE_TEST_FAIL_IMAGE_PUSH=true; then
    fail "release succeeded after the registry push failed"
  fi
  [[ -z "$(event_line create)" ]] || fail "failed image push reached GitHub publication"
  [[ -z "$(edit_line_with_flag --latest)" ]] || fail "failed image push changed latest"

  reset_case
  if run_release RELEASE_TEST_FAIL_ASSET_VERIFY=true; then
    fail "release succeeded after asset verification failed"
  fi
  [[ -n "$(edit_line_with_flag --draft=false)" ]] || fail "failure case never published the immutable release"
  [[ -z "$(edit_line_with_flag --latest)" ]] || fail "asset verification failure changed latest"
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
  if grep -F $'docker\tbuildx\tbuild' "$EVENTS" >/dev/null; then
    fail "post-build source changes reached the registry build"
  fi
}

test_local_build_leaves_one_usable_image() {
  reset_case
  release_env "$FIXTURE_ROOT/scripts/build-release.sh" "$FIXTURE_ROOT/dist/release"
  [[ -f "$TEST_STATE/image-loaded" ]] || fail "contributor build did not leave a usable local image"
  assert_application_artifacts
}

test_publish_verifies_before_latest_promotion
test_failed_publication_never_changes_latest
test_dirty_or_foreign_binary_never_publishes
test_local_build_leaves_one_usable_image

printf 'release tests passed\n'
