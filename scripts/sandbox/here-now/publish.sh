#!/usr/bin/env bash
set +x
set -euo pipefail

die() {
  printf 'error: %s\n' "$1" >&2
  exit 1
}

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly UPSTREAM="$SCRIPT_DIR/publish-upstream.sh"
readonly CREDENTIALS_DIR="${HOME:?HOME is required}/.herenow"
readonly CREDENTIALS_FILE="$CREDENTIALS_DIR/credentials"

[[ -f "$UPSTREAM" && ! -L "$UPSTREAM" ]] || die "the pinned here.now helper is unavailable"
[[ -z "${HERENOW_API_KEY:-}" && -z "${HERENOW_DRIVE_TOKEN:-}" ]] ||
  die "environment credentials are disabled; a trusted coordinator must provision the protected credential file"

args=("$@")
for ((index = 0; index < ${#args[@]}; index++)); do
  case "${args[$index]}" in
    --api-key|--api-key=*) die "command-line API keys are disabled" ;;
    --claim-token|--claim-token=*) die "claim-token publishing is disabled" ;;
    --base-url|--base-url=*) die "alternate here.now API bases are disabled" ;;
    --from-drive|--from-drive=*|--version|--version=*) die "Drive publishing is disabled" ;;
    --allow-nonherenow-base-url)
      die "non-default here.now API bases are disabled"
      ;;
  esac
done

[[ -d "$CREDENTIALS_DIR" && ! -L "$CREDENTIALS_DIR" ]] ||
  die "the here.now credential directory must be a private directory"
[[ "$(stat -c '%a' -- "$CREDENTIALS_DIR")" == "700" ]] ||
  die "the here.now credential directory must have mode 0700"
[[ "$(stat -c '%u' -- "$CREDENTIALS_DIR")" == "$(id -u)" ]] ||
  die "the here.now credential directory must be owned by the current user"
[[ -f "$CREDENTIALS_FILE" && ! -L "$CREDENTIALS_FILE" ]] ||
  die "a trusted coordinator must provision the here.now credential file"
[[ "$(stat -c '%a' -- "$CREDENTIALS_FILE")" == "600" ]] ||
  die "the here.now credential file must have mode 0600"
[[ "$(stat -c '%u' -- "$CREDENTIALS_FILE")" == "$(id -u)" ]] ||
  die "the here.now credential file must be owned by the current user"
[[ -s "$CREDENTIALS_FILE" ]] || die "the here.now credential file is empty"
credential="$(<"$CREDENTIALS_FILE")"
[[ "$credential" =~ ^hnk_[A-Za-z0-9_-]+$ ]] || die "the here.now credential file is invalid"

if git rev-parse --is-inside-work-tree >/dev/null 2>&1 &&
  ! git check-ignore -q -- .herenow/state.json; then
  die ".herenow/state.json must be ignored before publishing from a Git worktree"
fi

protect_state() {
  if [[ -L .herenow ]]; then
    die ".herenow must not be a symlink"
  fi
  if [[ -e .herenow && ! -d .herenow ]]; then
    die ".herenow must be a directory"
  fi
  if [[ -d .herenow ]]; then
    chmod 0700 .herenow
  fi
  if [[ -L .herenow/state.json ]]; then
    die ".herenow/state.json must not be a symlink"
  fi
  if [[ -e .herenow/state.json && ! -f .herenow/state.json ]]; then
    die ".herenow/state.json must be a regular file"
  fi
  if [[ -f .herenow/state.json ]]; then
    chmod 0600 .herenow/state.json
  fi
}

protect_state
umask 077
temporary="$(mktemp -d "${TMPDIR:-/tmp}/dorf-here-now.XXXXXXXX")"
cleanup() {
  rm -f -- "$temporary/stdout" "$temporary/stderr" "$temporary/authorization"
  rmdir -- "$temporary" 2>/dev/null || true
}
trap cleanup EXIT

# curl accepts headers from a file. Keeping the value out of argv prevents it
# from appearing in process listings; the patched, pinned helper requires this file.
{
  printf 'authorization: Bearer %s\n' "$credential"
} >"$temporary/authorization"
unset credential

status=0
env -u HERENOW_API_KEY -u HERENOW_DRIVE_TOKEN -u SHELLOPTS -u BASHOPTS -u BASH_ENV \
  DORF_HERENOW_AUTH_HEADER_FILE="$temporary/authorization" \
  bash "$UPSTREAM" "$@" >"$temporary/stdout" 2>"$temporary/stderr" || status=$?

protect_state
if [[ "$status" -ne 0 ]]; then
  die "here.now publish failed with exit $status; vendor output was withheld because it can contain bearer capabilities"
fi

result_field() {
  local name="$1"
  sed -n "s/^publish_result\.${name}=//p" "$temporary/stderr" | tail -n 1
}

site_url="$(result_field site_url)"
slug="$(result_field slug)"
action="$(result_field action)"
auth_mode="$(result_field auth_mode)"
persistence="$(result_field persistence)"

[[ "$slug" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || die "here.now returned invalid publish metadata"
[[ "$site_url" == "https://${slug}.here.now/" ]] || die "here.now returned an unexpected site URL"
[[ "$action" == "create" || "$action" == "update" ]] || die "here.now returned an invalid publish action"
[[ "$auth_mode" == "authenticated" ]] || die "here.now did not confirm an authenticated publish"
[[ "$persistence" == "permanent" || "$persistence" == "expires_at" ]] ||
  die "here.now returned invalid persistence metadata"

printf '%s\n' "$site_url"
printf 'publish_result.site_url=%s\n' "$site_url" >&2
printf 'publish_result.slug=%s\n' "$slug" >&2
printf 'publish_result.action=%s\n' "$action" >&2
printf 'publish_result.auth_mode=%s\n' "$auth_mode" >&2
printf 'publish_result.persistence=%s\n' "$persistence" >&2
