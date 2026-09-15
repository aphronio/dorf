#!/usr/bin/env bash
set -euo pipefail
readonly recipe_dir="$(dirname -- "$(readlink -f -- "${BASH_SOURCE[0]}")")"
readonly profile=/nix/var/nix/profiles/dorf-runner
export NIX_CONFIG=$'experimental-features = nix-command flakes\nbuild-users-group =\nsandbox = true'
export NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

bootstrap() (
  local version url digest archive temporary
  version=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nix"]["version"])' "$recipe_dir/packages.json")
  if [[ -x /root/.nix-profile/bin/nix ]]; then
    [[ $(/root/.nix-profile/bin/nix --version) == "nix (Nix) $version" ]]
    return
  fi
  url=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nix"]["url"])' "$recipe_dir/packages.json")
  digest=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nix"]["sha256"])' "$recipe_dir/packages.json")
  temporary=$(mktemp -d)
  trap 'rm -rf -- "$temporary"' EXIT
  archive="$temporary/nix-$version.tar.xz"
  curl --fail --location --retry 2 --max-time 180 "$url" --output "$archive"
  printf '%s  %s\n' "$digest" "$archive" | sha256sum --check --strict
  tar -xJf "$archive" -C "$temporary"
  mkdir -p /nix
  "$temporary/nix-$version-x86_64-linux/install" --no-daemon --no-channel-add --no-modify-profile
  /root/.nix-profile/bin/nix --version
)

stage() {
  local version=${1:?exact Codex version required} path
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 2
  mkdir -p "$recipe_dir/generations"
  path=$(/root/.nix-profile/bin/nix-build "$recipe_dir/package.nix" --argstr version "$version" --out-link "$recipe_dir/generations/$version")
  "$path/bin/codex" --version
}

activate() {
  local version=${1:?exact Codex version required} path
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 2
  path=$(readlink -e "$recipe_dir/generations/$version")
  [[ "$path" == /nix/store/* ]] || { echo 'Package must be staged before activation' >&2; exit 1; }
  [[ $("$path/bin/codex" --version) == "codex-cli $version" ]]
  /root/.nix-profile/bin/nix-env --profile "$profile" --set "$path"
  ln -sfn "$profile/bin/codex" /usr/local/bin/.codex-upgrade
  mv -Tf /usr/local/bin/.codex-upgrade /usr/local/bin/codex
  "$profile/bin/codex" --version
}

case ${1:-} in
  bootstrap) bootstrap ;;
  default-version) python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["default_codex"])' "$recipe_dir/packages.json" ;;
  stage) stage "${2:-}" ;;
  activate) activate "${2:-}" ;;
  inspect) readlink -f "$profile"; "$profile/bin/codex" --version ;;
  source-hash) /root/.nix-profile/bin/nix-prefetch-url --unpack "https://github.com/NixOS/nixpkgs/archive/$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nixpkgs"]["revision"])' "$recipe_dir/packages.json").tar.gz" ;;
  *) echo 'usage: guest.sh bootstrap|stage VERSION|activate VERSION|inspect|source-hash' >&2; exit 2 ;;
esac
