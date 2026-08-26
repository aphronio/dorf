#!/bin/sh
set -eu

DEFAULT_VERSION="@DORF_VERSION@"
DORF_RELEASES_URL="${DORF_RELEASES_URL:-https://github.com/aphronio/dorf/releases}"

usage() {
  cat <<'EOF'
Usage: install.sh [--version vX.Y.Z] [--install-dir ABSOLUTE_DIR] [--update]

Install a verified Dorf release for x86_64 Linux. The default install directory is
$HOME/.local/bin. The release asset supplies the default version; --version selects
another exact immutable release. --update omits fresh-install setup guidance.
EOF
}

fail() {
  printf 'dorf installer: %s\n' "$*" >&2
  exit 1
}

version="$DEFAULT_VERSION"
install_dir="${DORF_INSTALL_DIR:-}"
update=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --version)
      [ "$#" -ge 2 ] || fail "--version requires vX.Y.Z"
      version="$2"
      shift 2
      ;;
    --install-dir)
      [ "$#" -ge 2 ] || fail "--install-dir requires an absolute directory"
      install_dir="$2"
      shift 2
      ;;
    --update)
      update=true
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      fail "unknown option: $1"
      ;;
  esac
done

[ "$(uname -s)" = "Linux" ] || fail "supported platform is Linux x86_64"
[ "$(uname -m)" = "x86_64" ] || fail "supported platform is Linux x86_64"
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' ||
  fail "version must have the form vX.Y.Z"

for command in awk chmod curl grep install mkdir mktemp mv rm sed sha256sum tar uname; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable: $command"
done

if [ -z "$install_dir" ]; then
  [ -n "${HOME:-}" ] || fail "HOME is unset; pass --install-dir ABSOLUTE_DIR"
  install_dir="$HOME/.local/bin"
fi
[ -n "$install_dir" ] || fail "install directory must not be empty"
case "$install_dir" in
  /*) ;;
  *) fail "install directory must be an absolute path" ;;
esac

product_version="${version#v}"
archive="dorf_${product_version}_linux_x86_64.tar.gz"
checksums="dorf_${product_version}_checksums.txt"
download_base="${DORF_RELEASES_URL%/}/download/$version"
work_dir="$(mktemp -d)"
binary_temp=""
compose_temp=""
incus_compose_temp=""
cleanup() {
  [ -z "$binary_temp" ] || rm -f -- "$binary_temp"
  [ -z "$compose_temp" ] || rm -f -- "$compose_temp"
  [ -z "$incus_compose_temp" ] || rm -f -- "$incus_compose_temp"
  rm -rf -- "$work_dir"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

printf 'Downloading Dorf %s...\n' "$product_version"
curl -fsSL -o "$work_dir/$archive" "$download_base/$archive"
curl -fsSL -o "$work_dir/$checksums" "$download_base/$checksums"

expected_checksum="$(
  awk -v file="$archive" '
    $2 == file || $2 == "*" file { count += 1; checksum = $1 }
    END { if (count == 1) print checksum; else exit 1 }
  ' "$work_dir/$checksums"
)" || fail "checksum file does not identify exactly one $archive"
(
  cd "$work_dir"
  printf '%s  %s\n' "$expected_checksum" "$archive" | sha256sum --check --strict -
) || fail "checksum verification failed for $archive"

mkdir -p "$work_dir/extract"
tar -xzf "$work_dir/$archive" -C "$work_dir/extract" \
  dorf dorf-compose.yaml dorf-compose-incus.yaml ||
  fail "release archive does not contain the complete Dorf installation"
[ -f "$work_dir/extract/dorf" ] || fail "release archive does not contain dorf"
[ -f "$work_dir/extract/dorf-compose.yaml" ] ||
  fail "release archive does not contain dorf-compose.yaml"
[ -f "$work_dir/extract/dorf-compose-incus.yaml" ] ||
  fail "release archive does not contain dorf-compose-incus.yaml"
chmod 0755 "$work_dir/extract/dorf"
chmod 0644 "$work_dir/extract/dorf-compose.yaml" "$work_dir/extract/dorf-compose-incus.yaml"
observed_version="$("$work_dir/extract/dorf" version)" || fail "downloaded dorf could not report its version"
[ "$observed_version" = "dorf $product_version" ] ||
  fail "downloaded binary reported '$observed_version', expected 'dorf $product_version'"

mkdir -p "$install_dir" || fail "cannot create $install_dir; pass --install-dir ABSOLUTE_DIR"
target="$install_dir/dorf"
compose_target="$install_dir/dorf-compose.yaml"
incus_compose_target="$install_dir/dorf-compose-incus.yaml"
[ ! -d "$target" ] || fail "$target is a directory"
[ ! -d "$compose_target" ] || fail "$compose_target is a directory"
[ ! -d "$incus_compose_target" ] || fail "$incus_compose_target is a directory"
existing_install=false
if [ -e "$target" ]; then
  existing_install=true
fi
binary_temp="$(mktemp "$install_dir/.dorf.install.XXXXXX")" ||
  fail "cannot write to $install_dir; pass --install-dir ABSOLUTE_DIR"
compose_temp="$(mktemp "$install_dir/.dorf-compose.install.XXXXXX")" ||
  fail "cannot write to $install_dir; pass --install-dir ABSOLUTE_DIR"
incus_compose_temp="$(mktemp "$install_dir/.dorf-compose-incus.install.XXXXXX")" ||
  fail "cannot write to $install_dir; pass --install-dir ABSOLUTE_DIR"
install -m 0755 "$work_dir/extract/dorf" "$binary_temp"
install -m 0644 "$work_dir/extract/dorf-compose.yaml" "$compose_temp"
install -m 0644 "$work_dir/extract/dorf-compose-incus.yaml" "$incus_compose_temp"
mv -f -- "$compose_temp" "$compose_target"
compose_temp=""
mv -f -- "$incus_compose_temp" "$incus_compose_target"
incus_compose_temp=""
mv -f -- "$binary_temp" "$target"
binary_temp=""

installed_version="$("$target" version)" || fail "installed dorf could not report its version"
[ "$installed_version" = "dorf $product_version" ] ||
  fail "installed binary reported '$installed_version', expected 'dorf $product_version'"

printf 'Installed %s at %s\n' "$installed_version" "$target"
case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *)
    printf 'Add Dorf to this shell PATH:\n  export PATH='
    printf "'%s'" "$(printf '%s' "$install_dir" | sed "s/'/'\\\\''/g")"
    printf ':"$PATH"\n'
    ;;
esac
if [ "$update" = false ] && [ "$existing_install" = false ]; then
  printf 'Next, initialize Dorf when you are ready:\n  dorf setup\n'
fi
