#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly BUN_VERSION="1.3.5"

if [[ $# -gt 1 ]] || [[ $# -eq 1 && "$1" != "--check" ]]; then
  echo "usage: $0 [--check]" >&2
  exit 2
fi

if ! command -v bun >/dev/null 2>&1; then
  echo "E2B template construction requires Bun $BUN_VERSION." >&2
  exit 1
fi
if [[ "$(bun --version)" != "$BUN_VERSION" ]]; then
  echo "E2B template construction requires Bun $BUN_VERSION; observed $(bun --version)." >&2
  exit 1
fi

cd "$PROJECT_ROOT"
# Use Bun's dotenv loading for both preflight and build. Exported values take precedence.
bun -e 'if (!process.env.E2B_API_KEY) { console.error("Set E2B_API_KEY in the repository .env or export it before building."); process.exit(1); } console.log("E2B build credential is configured (value hidden).");'
if [[ "${1:-}" == "--check" ]]; then
  exit 0
fi

cd "$SCRIPT_DIR"
bun install --frozen-lockfile
cd "$PROJECT_ROOT"
exec bun run scripts/e2b/build-template.ts
