#!/usr/bin/env bash
set -euo pipefail
readonly root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
bun install --cwd scripts/e2b --frozen-lockfile >/dev/null
mkdir -p .dorf/bin
mise exec -- go build -o .dorf/bin/runtime-upgrade-provider ./scripts/runtime-upgrade/provider.go
if [[ ${1:-} == cleanup ]]; then
  exec bun scripts/e2b/runtime-upgrade-cleanup.ts "${2:?exact proof run ID required}"
fi
: "${1:?provider: incus or e2b}"
exec bun scripts/e2b/runtime-upgrade-proof.ts "$@"
