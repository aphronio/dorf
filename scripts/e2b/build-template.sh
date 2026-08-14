#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly BUN_VERSION="1.3.5"

if ! command -v bun >/dev/null 2>&1; then
  echo "E2B template construction requires Bun $BUN_VERSION." >&2
  exit 1
fi
if [[ "$(bun --version)" != "$BUN_VERSION" ]]; then
  echo "E2B template construction requires Bun $BUN_VERSION; observed $(bun --version)." >&2
  exit 1
fi

cd "$SCRIPT_DIR"
bun install --frozen-lockfile
cd "$PROJECT_ROOT"
exec bun run scripts/e2b/build-template.ts
