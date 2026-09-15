#!/usr/bin/env bash
set -euo pipefail
readonly recipe_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly profile=/nix/var/nix/profiles/dorf-runner
export NIX_CONFIG=$'experimental-features = nix-command flakes\nbuild-users-group =\nsandbox = true'
export NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

bootstrap() {
  if [[ -x /root/.nix-profile/bin/nix ]]; then
    /root/.nix-profile/bin/nix --version
    return
  fi
  local version url digest archive
  version=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nix"]["version"])' "$recipe_dir/packages.json")
  url=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nix"]["url"])' "$recipe_dir/packages.json")
  digest=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nix"]["sha256"])' "$recipe_dir/packages.json")
  archive="$recipe_dir/nix-$version.tar.xz"
  curl --fail --location --retry 2 --max-time 180 "$url" --output "$archive"
  printf '%s  %s\n' "$digest" "$archive" | sha256sum --check --strict
  tar -xJf "$archive" -C "$recipe_dir"
  mkdir -p /nix
  "$recipe_dir/nix-$version-x86_64-linux/install" --no-daemon --no-channel-add --no-modify-profile
  /root/.nix-profile/bin/nix --version
}

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
  ln -sfn "$profile/bin/codex" /usr/local/bin/codex
  "$profile/bin/codex" --version
}

case ${1:-} in
  bootstrap) bootstrap ;;
  stage) stage "${2:-}" ;;
  activate) activate "${2:-}" ;;
  inspect) readlink -f "$profile"; "$profile/bin/codex" --version ;;
  source-hash) /root/.nix-profile/bin/nix-prefetch-url --unpack "https://github.com/NixOS/nixpkgs/archive/$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["nixpkgs"]["revision"])' "$recipe_dir/packages.json").tar.gz" ;;
  *) echo 'usage: guest.sh bootstrap|stage VERSION|activate VERSION|inspect|source-hash' >&2; exit 2 ;;
esac
