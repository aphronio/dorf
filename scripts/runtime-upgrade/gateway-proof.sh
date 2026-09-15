#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || ! "$1" =~ ^(incus|e2b)$ ]]; then
  echo 'usage: gateway-proof.sh incus|e2b /absolute/build.json' >&2
  exit 2
fi
: "${DORF_UPGRADE_GATEWAY_HOST:?Set the configured Gateway SSH host}"
: "${DORF_UPGRADE_GATEWAY_URL:?Set its existing guest-reachable HTTPS URL}"
if [[ ! "$DORF_UPGRADE_GATEWAY_HOST" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.@-]*$ ]]; then
  echo 'Invalid Gateway SSH host' >&2
  exit 2
fi
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
export DORF_UPGRADE_GATEWAY_HELPER="/tmp/dorf-upgrade-gateway-proof-$(date +%s)-$$"
helper="$root/.dorf/bin/runtime-upgrade-gateway"
mise exec -- go build -o "$helper" ./scripts/runtime-upgrade/gateway
scp -q -o BatchMode=yes -o ConnectTimeout=10 "$helper" "$DORF_UPGRADE_GATEWAY_HOST:$DORF_UPGRADE_GATEWAY_HELPER"
# The test revokes its exact synthetic consumer route through coordinated cleanup.
# Keep the helper on a failed proof so the same authority can finish cleanup.
mise run integration:nix-image verify "$1" "$2"
ssh -T -o BatchMode=yes -o ConnectTimeout=10 -o RemoteCommand=none \
  "$DORF_UPGRADE_GATEWAY_HOST" "rm -- $DORF_UPGRADE_GATEWAY_HELPER"
