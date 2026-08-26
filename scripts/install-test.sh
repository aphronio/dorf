#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE_INSTALLER="$PROJECT_ROOT/scripts/install.sh"
WORK_DIR="$(mktemp -d)"
INSTALLER="$WORK_DIR/install.sh"
RELEASES_DIR="$WORK_DIR/releases"
SERVER_PID=""
GENERATED_INSTALLER="${1:-}"
GENERATED_ASSETS="${2:-}"
GENERATED_VERSION=""

cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf -- "$WORK_DIR"
}
trap cleanup EXIT

fail() {
  printf 'installer test failed: %s\n' "$*" >&2
  exit 1
}

assert_binary_output() {
  local binary="$1"
  local expected="$2"
  local actual

  [[ -x "$binary" ]] || fail "expected executable $binary"
  actual="$("$binary" version)"
  [[ "$actual" == "$expected" ]] ||
    fail "$binary printed '$actual', expected '$expected'"
}

create_release() {
  local version="$1"
  local output="$2"
  local checksum="$3"
  local artifact_basename="dorf_${version}_linux_x86_64"
  local archive="${artifact_basename}.tar.gz"
  local container_archive="${artifact_basename}_container-image.docker.tar"
  local release_dir="$RELEASES_DIR/download/v$version"
  local stage="$WORK_DIR/stage-$version"

  mkdir -p "$release_dir" "$stage"
  printf '#!/bin/sh\n[ "${1:-}" = version ] || exit 2\nprintf "%%s\\n" "%s"\n' \
    "$output" >"$stage/dorf"
  chmod 0755 "$stage/dorf"
  tar -C "$stage" -czf "$release_dir/$archive" dorf
  printf '%s\n' "unrelated container archive for checksum selection" \
    >"$release_dir/$container_archive"

  if [[ "$checksum" == valid ]]; then
    (
      cd "$release_dir"
      # Keep the unrelated asset first so a successful install proves the installer
      # selects the one exact application-archive line rather than the first line.
      sha256sum "$container_archive" "$archive" >"dorf_${version}_checksums.txt"
    )
  else
    (
      cd "$release_dir"
      sha256sum "$container_archive" >"dorf_${version}_checksums.txt"
      printf '%064d  %s\n' 0 "$archive" >>"dorf_${version}_checksums.txt"
    )
  fi
}

prepare_generated_release() {
  local archive_path
  local release_dir

  if [[ -z "$GENERATED_INSTALLER" ]]; then
    return 0
  fi
  [[ -n "$GENERATED_ASSETS" ]] || fail "generated installer assets directory is required"
  [[ -x "$GENERATED_INSTALLER" ]] || fail "generated installer is not executable: $GENERATED_INSTALLER"
  GENERATED_VERSION="$(sed -n 's/^DEFAULT_VERSION="v\([0-9][0-9.]*\)"$/\1/p' \
    "$GENERATED_INSTALLER")"
  [[ -n "$GENERATED_VERSION" ]] || fail "generated installer release version is missing"
  archive_path="$GENERATED_ASSETS/dorf_${GENERATED_VERSION}_linux_x86_64.tar.gz"
  [[ -f "$archive_path" ]] || fail "generated Go release archive is missing: $archive_path"
  release_dir="$RELEASES_DIR/download/v$GENERATED_VERSION"
  mkdir -p "$release_dir"
  cp "$archive_path" "$GENERATED_ASSETS/dorf_${GENERATED_VERSION}_checksums.txt" "$release_dir/"
}

start_release_server() {
  local port_file="$WORK_DIR/server-port"
  local server_log="$WORK_DIR/server.log"
  local attempt

  python3 - "$RELEASES_DIR" "$port_file" >"$server_log" 2>&1 <<'PY' &
import http.server
import pathlib
import sys

root = sys.argv[1]
port_file = pathlib.Path(sys.argv[2])

class QuietServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=root, **kwargs)

    def log_message(self, format, *args):
        pass

server = QuietServer(("127.0.0.1", 0), QuietHandler)
port_file.write_text(str(server.server_address[1]), encoding="utf-8")
server.serve_forever()
PY
  SERVER_PID=$!

  for attempt in {1..100}; do
    if [[ -s "$port_file" ]]; then
      RELEASES_URL="http://127.0.0.1:$(<"$port_file")"
      if curl -fsS "$RELEASES_URL/download/v1.2.3/dorf_1.2.3_checksums.txt" >/dev/null; then
        return
      fi
    fi
    sleep 0.05
  done

  sed -n '1,120p' "$server_log" >&2
  fail "local release server did not become ready"
}

install_with_release_server() {
  DORF_RELEASES_URL="$RELEASES_URL" \
    sh "$INSTALLER" "$@"
}

test_pinned_default_and_explicit_replacement() {
  local install_dir="$WORK_DIR/explicit-bin"

  DORF_DEFAULT_VERSION=v2.4.6 DORF_INSTALL_DIR="$install_dir" install_with_release_server
  assert_binary_output "$install_dir/dorf" "dorf 1.2.3"

  install_with_release_server --version v2.4.6 --install-dir "$install_dir"
  assert_binary_output "$install_dir/dorf" "dorf 2.4.6"

  install_with_release_server --version v2.4.6 --install-dir "$install_dir"
  assert_binary_output "$install_dir/dorf" "dorf 2.4.6"
}

test_fresh_install_and_update_guidance() {
  local install_dir="$WORK_DIR/guidance-bin"
  local install_output legacy_update_output update_output

  install_output="$(install_with_release_server --install-dir "$install_dir")"
  [[ "$install_output" == *$'Next, initialize Dorf when you are ready:\n  dorf setup'* ]] ||
    fail "standalone installer omitted fresh-install setup guidance"

  legacy_update_output="$(
    install_with_release_server --version v2.4.6 --install-dir "$install_dir"
  )"
  [[ "$legacy_update_output" == *"Installed dorf 2.4.6 at $install_dir/dorf"* ]] ||
    fail "legacy update caller omitted successful installation output"
  [[ "$legacy_update_output" != *"dorf setup"* ]] ||
    fail "legacy update caller printed fresh-install setup guidance"

  update_output="$(
    install_with_release_server --version v2.4.6 --install-dir "$install_dir" --update
  )"
  [[ "$update_output" == *"Installed dorf 2.4.6 at $install_dir/dorf"* ]] ||
    fail "update installer omitted successful installation output"
  [[ "$update_output" != *"dorf setup"* ]] ||
    fail "update installer printed fresh-install setup guidance"
  assert_binary_output "$install_dir/dorf" "dorf 2.4.6"
}

test_install_dir_must_be_absolute() {
  if install_with_release_server --install-dir relative-bin; then
    fail "installer accepted a relative install directory"
  fi
}

test_checksum_failure_is_atomic() {
  local install_dir="$WORK_DIR/checksum-bin"
  local unexpected

  mkdir -p "$install_dir"
  printf '%s\n' \
    '#!/bin/sh' \
    '[ "${1:-}" = version ] || exit 2' \
    'printf "%s\n" "existing-dorf"' >"$install_dir/dorf"
  chmod 0755 "$install_dir/dorf"

  if install_with_release_server --version v9.9.9 --install-dir "$install_dir"; then
    fail "installer accepted an archive with a bad checksum"
  fi

  assert_binary_output "$install_dir/dorf" existing-dorf
  unexpected="$(find "$install_dir" -mindepth 1 -maxdepth 1 ! -name dorf -print -quit)"
  [[ -z "$unexpected" ]] || fail "installer left a partial file: $unexpected"
}

test_unsupported_platforms() {
  local shim_dir="$WORK_DIR/platform-shim"

  mkdir -p "$shim_dir"
  printf '%s\n' \
    '#!/bin/sh' \
    'case "${1:-}" in' \
    '  -s) printf "%s\n" "$TEST_UNAME_S" ;;' \
    '  -m) printf "%s\n" "$TEST_UNAME_M" ;;' \
    '  *) exit 2 ;;' \
    'esac' >"$shim_dir/uname"
  chmod 0755 "$shim_dir/uname"

  if TEST_UNAME_S=Darwin TEST_UNAME_M=x86_64 PATH="$shim_dir:$PATH" \
    install_with_release_server --install-dir "$WORK_DIR/darwin-bin"; then
    fail "installer accepted unsupported macOS"
  fi

  if TEST_UNAME_S=Linux TEST_UNAME_M=aarch64 PATH="$shim_dir:$PATH" \
    install_with_release_server --install-dir "$WORK_DIR/arm-bin"; then
    fail "installer accepted unsupported Linux architecture"
  fi
}

test_default_target_path_handoff() {
  local test_home="$WORK_DIR/home"
  local default_bin="$test_home/.local/bin"
  local actual
  local installer_output

  mkdir -p "$test_home"
  installer_output="$(
    unset DORF_INSTALL_DIR
    HOME="$test_home" install_with_release_server
  )"
  assert_binary_output "$default_bin/dorf" "dorf 1.2.3"
  [[ "$installer_output" == *"export PATH='$default_bin':\"\$PATH\""* ]] ||
    fail "installer did not print the default-directory PATH handoff"
  actual="$(PATH="$default_bin:$PATH" dorf version)"
  [[ "$actual" == "dorf 1.2.3" ]] || fail "installed dorf was not available through PATH"
}

test_path_handoff_shell_quotes_install_dir() {
  local marker="$WORK_DIR/path-command-ran"
  local install_dir="$WORK_DIR/bin with ' quote;\$(touch $marker)"
  local installer_output handoff

  installer_output="$(install_with_release_server --install-dir "$install_dir")"
  handoff="$(printf '%s\n' "$installer_output" | sed -n 's/^  export PATH=//p')"
  [[ -n "$handoff" ]] || fail "installer omitted the custom-directory PATH handoff"
  eval "PATH=$handoff"
  [[ "$PATH" == "$install_dir:"* ]] || fail "PATH handoff did not preserve the install directory"
  [[ ! -e "$marker" ]] || fail "PATH handoff evaluated install-directory shell content"
}

test_generated_installer_embeds_release_version() {
  local install_dir="$WORK_DIR/generated-bin"

  if [[ -z "$GENERATED_INSTALLER" ]]; then
    return 0
  fi
  DORF_RELEASES_URL="$RELEASES_URL" DORF_INSTALL_DIR="$install_dir" sh "$GENERATED_INSTALLER"
  assert_binary_output "$install_dir/dorf" "dorf $GENERATED_VERSION"
}

[[ -f "$SOURCE_INSTALLER" ]] || fail "missing installer at $SOURCE_INSTALLER"
sed 's/@DORF_VERSION@/v1.2.3/g' "$SOURCE_INSTALLER" >"$INSTALLER"
chmod 0755 "$INSTALLER"
sh -n "$INSTALLER"
create_release 1.2.3 "dorf 1.2.3" valid
create_release 2.4.6 "dorf 2.4.6" valid
create_release 9.9.9 corrupt-dorf invalid
prepare_generated_release
start_release_server

test_pinned_default_and_explicit_replacement
test_fresh_install_and_update_guidance
test_install_dir_must_be_absolute
test_checksum_failure_is_atomic
test_unsupported_platforms
test_default_target_path_handoff
test_path_handoff_shell_quotes_install_dir
test_generated_installer_embeds_release_version

printf 'installer tests passed\n'
