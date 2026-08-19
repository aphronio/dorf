#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly MISE_VERSION="2026.8.5"
readonly MISE_SHA256="ee362b6d96c648e27325a8bc7ee866bde4fffc20c88c777c5eb5c3b5c6f3e226"
readonly MISE="$PROJECT_ROOT/.dorf/bin/mise"
readonly MISE_BINARY="$PROJECT_ROOT/.dorf/mise/bin/mise"
readonly OS_ID="$(. /etc/os-release; printf '%s' "${ID:-}")"
readonly POSTGRES_MAJOR="17"
readonly POSTGRES_SOCKET="/var/run/postgresql"
readonly TEST_DATABASE_FILE="$PROJECT_ROOT/.dorf/test-database-url"

if [[ -z "${DORF_TEST_DATABASE_URL:-}" && -f "$TEST_DATABASE_FILE" ]]; then
  DORF_TEST_DATABASE_URL="$(<"$TEST_DATABASE_FILE")"
  export DORF_TEST_DATABASE_URL
fi

as_root() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
    return
  fi
  if command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
    sudo -n -- "$@"
    return
  fi
  echo "Dorf development setup needs root or passwordless sudo to install missing bootstrap packages." >&2
  exit 1
}

ensure_bootstrap_packages() {
  local missing=false
  for command in curl sha256sum psql; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing=true
    fi
  done
  if [[ -z "${DORF_TEST_DATABASE_URL:-}" ]]; then
    if [[ "$OS_ID" != "debian" ]]; then
      echo "Automatic PostgreSQL setup requires a Debian workload; set DORF_TEST_DATABASE_URL on $OS_ID." >&2
      exit 1
    fi
    for command in createdb createuser pg_ctlcluster pg_isready runuser; do
      if ! command -v "$command" >/dev/null 2>&1; then
        missing=true
      fi
    done
  fi
  if [[ "$missing" == false ]]; then
    return
  fi
  if ! command -v apt-get >/dev/null 2>&1; then
    echo "Missing bootstrap tools and no supported apt package manager was found." >&2
    exit 1
  fi
  local packages=(ca-certificates coreutils curl postgresql-client)
  if [[ -z "${DORF_TEST_DATABASE_URL:-}" ]]; then
    packages+=(postgresql util-linux)
  fi
  as_root env DEBIAN_FRONTEND=noninteractive \
    apt-get -o DPkg::Lock::Timeout=120 update
  as_root env DEBIAN_FRONTEND=noninteractive \
    apt-get -o DPkg::Lock::Timeout=120 install -y --no-install-recommends \
      "${packages[@]}"
}

install_mise() {
  if [[ "$(uname -s)" != "Linux" || "$(uname -m)" != "x86_64" ]]; then
    echo "Dorf development setup currently supports Linux x86_64." >&2
    exit 1
  fi
  if [[ -x "$MISE_BINARY" ]] && [[ "$($MISE_BINARY version | awk '{print $1}')" == "$MISE_VERSION" ]]; then
    return
  fi
  local download
  download="$(mktemp)"
  trap 'rm -f -- "$download"' RETURN
  curl -LsSf -o "$download" \
    "https://github.com/jdx/mise/releases/download/v$MISE_VERSION/mise-v$MISE_VERSION-linux-x64"
  printf '%s  %s\n' "$MISE_SHA256" "$download" | sha256sum --check --strict
  install -D -m 0755 "$download" "$MISE_BINARY"
}

install_mise_wrapper() {
  mkdir -p "$PROJECT_ROOT/.dorf/bin"
  cat > "$MISE" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
export MISE_CONFIG_DIR="$project_root/.dorf/mise-config"
export MISE_DATA_DIR="$project_root/.dorf/mise-data"
export MISE_CACHE_DIR="$project_root/.dorf/mise-cache"
export MISE_STATE_DIR="$project_root/.dorf/mise-state"
exec "$project_root/.dorf/mise/bin/mise" "$@"
EOF
  chmod 0755 "$MISE"
}

mise_run() {
  "$MISE" -C "$PROJECT_ROOT" "$@"
}

mise_exec() {
  mise_run exec -- "$@"
}

postgres_admin() {
  as_root runuser -u postgres -- "$@"
}

converge_debian_database() {
  if ! pg_isready -q -h "$POSTGRES_SOCKET"; then
    as_root pg_ctlcluster "$POSTGRES_MAJOR" main start
  fi
  local database_user
  database_user="$(id -un)"
  if ! psql -d postgres -Atqc 'select 1' 2>/dev/null | grep -qx 1; then
    postgres_admin createuser "$database_user"
  fi
  if ! postgres_admin psql -d postgres -Atqc \
    "select 1 from pg_database where datname='dorf_test'" | grep -qx 1; then
    postgres_admin createdb --owner="$database_user" dorf_test
  fi
  printf 'postgresql:///dorf_test?host=%s\n' "$POSTGRES_SOCKET"
}

ensure_bootstrap_packages
install_mise
install_mise_wrapper
mkdir -p "$PROJECT_ROOT/.dorf/mise-config"
mise_run trust -y "$PROJECT_ROOT/.mise.toml" >/dev/null
mise_run install --locked -y

test_database_url="${DORF_TEST_DATABASE_URL:-}"
if [[ -z "$test_database_url" ]]; then
  test_database_url="$(converge_debian_database)"
fi
mkdir -p "$PROJECT_ROOT/.dorf"
temporary_database_url="$(mktemp "$PROJECT_ROOT/.dorf/.test-database-url.XXXXXX")"
printf '%s\n' "$test_database_url" > "$temporary_database_url"
chmod 0600 "$temporary_database_url"
mv -f -- "$temporary_database_url" "$TEST_DATABASE_FILE"

if ! psql "$test_database_url" -Atqc 'select 1' | grep -qx 1; then
  echo "Cannot connect to the configured disposable Dorf test database." >&2
  exit 1
fi
if ! mise_exec absurdctl schema-version -d "$test_database_url" >/dev/null 2>&1; then
  mise_exec absurdctl init -d "$test_database_url"
fi

mise_exec go mod download
DORF_DATABASE_URL="$test_database_url" mise_exec go run ./cmd/dorf migrate

printf '%s\n' \
  "Dorf development setup complete." \
  "  Mise: $($MISE version)" \
  "  Go: $(mise_exec go version)" \
  "  sqlc: $(mise_exec sqlc version)" \
  "  PostgreSQL: $(psql --version)" \
  "  DORF_TEST_DATABASE_URL configured" \
  "Run checks with: .dorf/bin/mise run check"
