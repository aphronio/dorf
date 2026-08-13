#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly MISE_VERSION="2026.8.5"
readonly MISE_SHA256="ee362b6d96c648e27325a8bc7ee866bde4fffc20c88c777c5eb5c3b5c6f3e226"
readonly MISE="$PROJECT_ROOT/.dorf/bin/mise"
readonly MISE_BINARY="$PROJECT_ROOT/.dorf/mise/bin/mise"
readonly DEFAULT_DATABASE_DIR="$PROJECT_ROOT/.dorf/postgres"

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
  for command in bison curl flex gcc make perl pkg-config runuser sha256sum useradd; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing=true
    fi
  done
  if command -v pkg-config >/dev/null 2>&1; then
    for library in openssl readline uuid zlib; do
      if ! pkg-config --exists "$library"; then
        missing=true
      fi
    done
  fi
  if [[ "$missing" == false ]]; then
    return
  fi
  if ! command -v apt-get >/dev/null 2>&1; then
    echo "Missing bootstrap tools and no supported apt-get package manager was found." >&2
    exit 1
  fi
  as_root env DEBIAN_FRONTEND=noninteractive \
    apt-get -o DPkg::Lock::Timeout=120 update
  as_root env DEBIAN_FRONTEND=noninteractive \
    apt-get -o DPkg::Lock::Timeout=120 install -y --no-install-recommends \
      bison build-essential ca-certificates curl flex libreadline-dev libssl-dev passwd pkg-config util-linux uuid-dev zlib1g-dev
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

ensure_postgres_user() {
  if [[ "$(id -u)" -eq 0 ]] && ! id dorf-postgres >/dev/null 2>&1; then
    useradd --system --home-dir /nonexistent --shell /usr/sbin/nologin dorf-postgres
  fi
}

postgres_exec() {
  local command=$1
  shift
  if [[ "$(id -u)" -ne 0 ]]; then
    mise_exec "$command" "$@"
    return
  fi
  ensure_postgres_user
  local executable
  executable="$(mise_run which "$command")"
  runuser -u dorf-postgres -- "$executable" "$@"
}

converge_local_database() {
  local data="$DEFAULT_DATABASE_DIR/data"
  local socket="$DEFAULT_DATABASE_DIR/socket"
  local log="$DEFAULT_DATABASE_DIR/postgres.log"
  mkdir -p "$socket"
  if [[ "$(id -u)" -eq 0 ]]; then
    ensure_postgres_user
    chown -R dorf-postgres:nogroup "$DEFAULT_DATABASE_DIR"
  fi
  if [[ ! -s "$data/PG_VERSION" ]]; then
    postgres_exec initdb -D "$data" --auth=trust --no-locale >&2
  fi
  if ! postgres_exec pg_ctl -D "$data" status >/dev/null 2>&1; then
    postgres_exec pg_ctl -D "$data" -l "$log" -o "-k $socket" -w start >&2
  fi
  local url="postgresql://dorf-postgres@/dorf_test?host=$socket"
  local admin_url="postgresql://dorf-postgres@/postgres?host=$socket"
  if ! postgres_exec psql "$admin_url" -Atqc \
    "select 1 from pg_database where datname='dorf_test'" | grep -qx 1; then
    postgres_exec createdb -h "$socket" -U dorf-postgres dorf_test >&2
  fi
  printf '%s\n' "$url"
}

ensure_bootstrap_packages
install_mise
install_mise_wrapper
mkdir -p "$PROJECT_ROOT/.dorf/mise-config"
mise_run trust -y "$PROJECT_ROOT/.mise.toml" >/dev/null
mise_run install --locked -y

test_database_url="${DORF_TEST_DATABASE_URL:-}"
if [[ -z "$test_database_url" ]]; then
  test_database_url="$(converge_local_database)"
fi
mkdir -p "$PROJECT_ROOT/.dorf"
printf '%s\n' "$test_database_url" > "$PROJECT_ROOT/.dorf/test-database-url"

if ! mise_exec psql "$test_database_url" -Atqc 'select 1' | grep -qx 1; then
  echo "Cannot connect to the disposable Dorf test database: $test_database_url" >&2
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
  "  PostgreSQL: $(mise_exec psql --version)" \
  "  DORF_TEST_DATABASE_URL=$test_database_url" \
  "Run checks with: .dorf/bin/mise run check"
