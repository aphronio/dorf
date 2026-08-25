#!/usr/bin/env bash
set -euo pipefail
readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)"
readonly API_UNIT="dorf-dogfood-control-api.service"
readonly WORKER_UNIT="dorf-dogfood-worker.service"
readonly API_ME_URL="http://127.0.0.1:8745/v1/me"
readonly READINESS_CREDENTIAL="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
declare DORF_BINARY SERVICE_HOME
fail() { printf 'Dorf control services: %s\n' "$*" >&2; exit 1; }
usage() {
  cat >&2 <<'EOF'
Usage: scripts/dev/control-services.sh up
       scripts/dev/control-services.sh status
       scripts/dev/control-services.sh logs api|worker
       scripts/dev/control-services.sh down

Creates disposable systemd services for dogfood on the deployment host.
EOF
  exit 2
}
require_root() { [[ "$(id -u)" -eq 0 ]] || fail "run this command as the deployment host's root operator"; }
unit_loaded() {
  local state
  state="$(systemctl show --property=LoadState --value "$1" 2>/dev/null || true)"
  [[ -n "$state" && "$state" != "not-found" ]]
}
assert_disposable() {
  unit_loaded "$1" || return 0
  [[ "$(systemctl show --property=Transient --value "$1")" == "yes" ]] ||
    fail "refusing to touch non-transient unit $1"
}
remove_unit() {
  unit_loaded "$1" || return 0
  systemctl stop "$1"
  for _ in {1..40}; do
    unit_loaded "$1" || return 0
    systemctl reset-failed "$1" >/dev/null 2>&1 || true
    sleep 0.1
  done
  fail "$1 did not unload"
}
clear_units() {
  assert_disposable "$API_UNIT"
  assert_disposable "$WORKER_UNIT"
  remove_unit "$API_UNIT"
  remove_unit "$WORKER_UNIT"
}
prepare_up() {
  local expected="$PROJECT_ROOT/.dorf/bin/dorf"
  [[ -x "$expected" ]] ||
    fail "build first: .dorf/bin/mise exec -- go build -o .dorf/bin/dorf ./cmd/dorf"
  DORF_BINARY="$(realpath -e -- "$expected")"
  [[ "$DORF_BINARY" == "$expected" ]] || fail "refusing linked binary at $expected"
  SERVICE_HOME="$(realpath -e -- "${HOME:-/root}")"
  [[ -d "$SERVICE_HOME" ]] || fail "deployment HOME is not a directory: $SERVICE_HOME"
  HOME="$SERVICE_HOME" "$DORF_BINARY" version >/dev/null || fail "repository Dorf binary did not start"
  HOME="$SERVICE_HOME" "$DORF_BINARY" migrate >/dev/null || fail "Dorf database migration failed"
}
start_unit() {
  local unit="$1" description="$2"
  local -a no_expand=()
  shift 2
  [[ "$(systemd-run --help)" == *"--expand-environment"* ]] &&
    no_expand+=(--expand-environment=no)
  systemd-run --quiet --collect --unit="$unit" --description="$description" \
    --service-type=exec --property=Restart=always --property=RestartSec=2s \
    --property=TimeoutStopSec=20s --working-directory="$PROJECT_ROOT" \
    --setenv="HOME=$SERVICE_HOME" "${no_expand[@]}" "$DORF_BINARY" "$@"
}
wait_active() {
  local stable=0
  for _ in {1..40}; do
    if systemctl is-active --quiet "$1"; then stable=$((stable + 1)); else stable=0; fi
    [[ "$stable" -eq 4 ]] && return
    sleep 0.25
  done
  systemctl --no-pager --full status "$1" >&2 || true
  fail "$1 did not remain active"
}
wait_api() {
  local response status body
  for _ in {1..40}; do
    response="$(curl --silent --max-time 1 --header "Authorization: Bearer $READINESS_CREDENTIAL" \
      --write-out $'\n%{http_code}' "$API_ME_URL" 2>/dev/null || true)"
    status="${response##*$'\n'}"; body="${response%$'\n'*}"
    [[ "$status" == "401" && "$body" == *'"code":"unauthenticated"'* ]] && return
    systemctl is-active --quiet "$API_UNIT" || break
    sleep 0.25
  done
  systemctl --no-pager --full status "$API_UNIT" >&2 || true
  fail "control API authentication did not become ready at $API_ME_URL"
}
unit_for() {
  case "${1:-}" in api) printf '%s\n' "$API_UNIT" ;; worker) printf '%s\n' "$WORKER_UNIT" ;; *) usage ;; esac
}
case "${1:-}" in
  up)
    [[ "$#" -eq 1 ]] || usage; require_root; prepare_up; clear_units
    trap 'systemctl stop "$API_UNIT" "$WORKER_UNIT" >/dev/null 2>&1 || true' EXIT
    start_unit "$WORKER_UNIT" "Dorf dogfood Job worker" worker
    start_unit "$API_UNIT" "Dorf dogfood control API" serve --listen 127.0.0.1:8745
    wait_active "$WORKER_UNIT"; wait_api; trap - EXIT
    printf 'Dogfood control API and worker are ready and survive SSH logout.\n'
    printf 'They are not enabled across reboot; HTTPS ingress was not changed.\n'
    printf 'Restart directly: systemctl restart %s %s\n' "$API_UNIT" "$WORKER_UNIT"
    ;;
  status) [[ "$#" -eq 1 ]] || usage; systemctl --no-pager --full status "$API_UNIT" "$WORKER_UNIT" ;;
  logs) [[ "$#" -eq 2 ]] || usage; unit="$(unit_for "$2")"; journalctl --no-pager --unit="$unit" --lines=200 ;;
  down) [[ "$#" -eq 1 ]] || usage; require_root; clear_units; printf 'Dogfood control services stopped.\n' ;;
  *) usage ;;
esac
