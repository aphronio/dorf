#!/usr/bin/env bash
set -euo pipefail

readonly GO_VERSION="1.26.5"
readonly GO_SHA256="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"
readonly ABSURD_VERSION="0.5.0"
readonly ABSURDCTL_SHA256="e9c37151bf3d1656fcc9b018dc90a2ac6972a31bda77329d8c929da34bb0724e"
readonly DEFAULT_TEST_DATABASE_URL="postgresql:///dorf_test?host=/var/run/postgresql"

temporary_directory=""
cleanup() {
  if [[ -n "$temporary_directory" ]]; then
    rm -rf -- "$temporary_directory"
  fi
}
trap cleanup EXIT

as_root() {
  if [[ "$(id -u)" -eq 0 ]]; then
    "$@"
    return
  fi
  if command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
    sudo -n -- "$@"
    return
  fi
  echo "Dorf repository preparation needs root or passwordless sudo to install missing tools." >&2
  exit 1
}

install_system_packages() {
  local missing=false
  for command in curl gcc jq pg_ctlcluster pg_isready psql createdb python3 rg; do
    if ! command -v "$command" >/dev/null 2>&1; then
      missing=true
    fi
  done
  if [[ "$missing" == false ]]; then
    return
  fi
  if ! command -v apt-get >/dev/null 2>&1; then
    echo "Missing Dorf development tools and no supported apt-get package manager was found." >&2
    exit 1
  fi
  as_root env DEBIAN_FRONTEND=noninteractive \
    apt-get -o DPkg::Lock::Timeout=120 update
  as_root env DEBIAN_FRONTEND=noninteractive \
    apt-get -o DPkg::Lock::Timeout=120 install -y --no-install-recommends \
      build-essential ca-certificates curl jq postgresql postgresql-client python3 ripgrep
}

install_go() {
  if command -v go >/dev/null 2>&1 && \
    [[ "$(go version | awk '{print $3}')" == "go$GO_VERSION" ]] && \
    command -v gofmt >/dev/null 2>&1; then
    return
  fi
  local archive="$temporary_directory/go.tar.gz"
  local target="/opt/dorf/toolchains/go-$GO_VERSION"
  curl -LsSf -o "$archive" "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
  printf '%s  %s\n' "$GO_SHA256" "$archive" | sha256sum --check --strict
  as_root install -d -m 0755 /opt/dorf/toolchains
  as_root rm -rf -- "$target"
  as_root install -d -m 0755 "$target"
  as_root tar --strip-components=1 -xzf "$archive" -C "$target"
  as_root ln -sfn "$target/bin/go" /usr/local/bin/go
  as_root ln -sfn "$target/bin/gofmt" /usr/local/bin/gofmt
  hash -r
}

install_absurdctl() {
  local installed=""
  if command -v absurdctl >/dev/null 2>&1; then
    installed="$(sha256sum "$(command -v absurdctl)" | awk '{print $1}')"
  fi
  if [[ "$installed" == "$ABSURDCTL_SHA256" ]]; then
    return
  fi
  local binary="$temporary_directory/absurdctl"
  curl -LsSf -o "$binary" \
    "https://github.com/earendil-works/absurd/releases/download/$ABSURD_VERSION/absurdctl"
  printf '%s  %s\n' "$ABSURDCTL_SHA256" "$binary" | sha256sum --check --strict
  as_root install -m 0755 "$binary" /usr/local/bin/absurdctl
  hash -r
}

run_as_postgres() {
  if [[ "$(id -u)" -eq 0 ]] && command -v runuser >/dev/null 2>&1; then
    runuser -u postgres -- "$@"
    return
  fi
  if command -v sudo >/dev/null 2>&1 && sudo -n -u postgres true >/dev/null 2>&1; then
    sudo -n -u postgres -- "$@"
    return
  fi
  return 1
}

ensure_local_test_database() {
  if ! pg_isready -q; then
    as_root systemctl start postgresql
  fi
  if ! pg_isready -q; then
    echo "Local PostgreSQL is not ready. Start it or set DORF_TEST_DATABASE_URL." >&2
    exit 1
  fi

  local developer
  developer="$(id -un)"
  if [[ ! "$developer" =~ ^[a-z_][a-z0-9_-]*$ ]]; then
    echo "Local account name is not safe for PostgreSQL role creation: $developer" >&2
    exit 1
  fi
  if ! run_as_postgres psql -d postgres -Atqc \
    "select 1 from pg_roles where rolname='$developer'" | grep -qx 1; then
    run_as_postgres createuser --createdb "$developer"
  fi
  if ! run_as_postgres psql -d postgres -Atqc \
    "select 1 from pg_database where datname='dorf_test'" | grep -qx 1; then
    run_as_postgres createdb --owner "$developer" dorf_test
  fi
}

temporary_directory="$(mktemp -d)"
install_system_packages
install_go
install_absurdctl

if [[ -z "${DORF_TEST_DATABASE_URL:-}" ]]; then
  ensure_local_test_database
fi
test_database_url="${DORF_TEST_DATABASE_URL:-$DEFAULT_TEST_DATABASE_URL}"

if ! psql "$test_database_url" -Atqc 'select 1' | grep -qx 1; then
  echo "Cannot connect to the disposable Dorf test database: $test_database_url" >&2
  exit 1
fi

schema_version="$(absurdctl schema-version -d "$test_database_url" 2>/dev/null || true)"
if ! grep -Eq '(^|[[:space:]])0\.5\.0($|[[:space:]])' <<<"$schema_version"; then
  absurdctl init -d "$test_database_url"
  schema_version="$(absurdctl schema-version -d "$test_database_url")"
fi
if ! grep -Eq '(^|[[:space:]])0\.5\.0($|[[:space:]])' <<<"$schema_version"; then
  echo "Dorf development requires Absurd schema $ABSURD_VERSION; observed: $schema_version" >&2
  exit 1
fi

go mod download

printf '%s\n' \
  "Dorf repository preparation complete." \
  "  Go: $(go version)" \
  "  Absurd schema: $ABSURD_VERSION" \
  "  PostgreSQL: $(psql --version)" \
  "  DORF_TEST_DATABASE_URL=$test_database_url"
