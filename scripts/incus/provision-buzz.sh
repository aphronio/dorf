#!/usr/bin/env bash
set -euo pipefail

BUZZ_VM="${BUZZ_VM:-dorf-buzz}"
BASE_IMAGE="${BASE_IMAGE:-images:ubuntu/24.04}"
NETWORK="${NETWORK:-incusbr0}"
ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
CPU_LIMIT="${CPU_LIMIT:-4}"
MEMORY_LIMIT="${MEMORY_LIMIT:-8GiB}"
BUZZ_IMAGE="${BUZZ_IMAGE:-ghcr.io/block/buzz:sha-2ce2d71}"
BUZZ_REVISION="${BUZZ_REVISION:-2ce2d71cc38a9657eaf344c10e07f155b8a18615}"
BUZZ_OWNER_PUBKEY="${BUZZ_OWNER_PUBKEY:-}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
GUEST_SCRIPT="$SCRIPT_DIR/provision-buzz-guest.sh"
NORMALIZE_PUBLIC_KEY="$SCRIPT_DIR/normalize-nostr-public-key.py"

vm_exists=false
if incus info "$BUZZ_VM" >/dev/null 2>&1; then
  vm_exists=true
fi

owner_public_key=""
if [[ -n "$BUZZ_OWNER_PUBKEY" ]]; then
  owner_public_key="$("$NORMALIZE_PUBLIC_KEY" "$BUZZ_OWNER_PUBKEY")"
elif [[ "$vm_exists" != true ]]; then
  cat >&2 <<'MSG'
BUZZ_OWNER_PUBKEY is required when creating the persistent Buzz VM.

Create and back up the human identity in Buzz Desktop, then pass the public
npub shown by the client:

  BUZZ_OWNER_PUBKEY=npub1... scripts/incus/provision-buzz.sh

The human private key must remain in the client and its human-controlled backup.
MSG
  exit 1
fi

if [[ "$vm_exists" != true ]]; then
  incus init "$BASE_IMAGE" "$BUZZ_VM" \
    --vm \
    --network "$NETWORK" \
    -d "root,size=$ROOT_DISK_SIZE"
fi

incus config set "$BUZZ_VM" \
  "limits.cpu=$CPU_LIMIT" \
  "limits.memory=$MEMORY_LIMIT" \
  "boot.autostart=true"

if [[ "$(incus list "$BUZZ_VM" --format csv -c s)" != "RUNNING" ]]; then
  incus start "$BUZZ_VM"
fi

guest_ready=false
for _ in {1..90}; do
  if incus exec "$BUZZ_VM" -- true >/dev/null 2>&1; then
    guest_ready=true
    break
  fi
  sleep 2
done

if [[ "$guest_ready" != true ]]; then
  echo "Timed out waiting for $BUZZ_VM guest agent" >&2
  exit 1
fi

if [[ -z "$owner_public_key" ]] &&
  ! incus exec "$BUZZ_VM" -- test -f /opt/dorf-buzz/source/deploy/compose/.env; then
  cat >&2 <<'MSG'
This Buzz VM has no deployment environment yet, so BUZZ_OWNER_PUBKEY is required.

Create and back up the human identity in Buzz Desktop, then rerun with its
public npub. No human private key belongs on the host or in the VM.
MSG
  exit 1
fi

incus file push "$GUEST_SCRIPT" "$BUZZ_VM/tmp/provision-buzz-guest.sh"
incus exec "$BUZZ_VM" -- chmod 0700 /tmp/provision-buzz-guest.sh
incus exec "$BUZZ_VM" \
  --env "BUZZ_IMAGE=$BUZZ_IMAGE" \
  --env "BUZZ_REVISION=$BUZZ_REVISION" \
  --env "BUZZ_OWNER_PUBKEY=$owner_public_key" \
  -- /tmp/provision-buzz-guest.sh
incus exec "$BUZZ_VM" -- rm -f /tmp/provision-buzz-guest.sh

echo
echo "Persistent Buzz VM is ready: $BUZZ_VM"
incus list "$BUZZ_VM" --format csv -c ns4m
echo "Next: scripts/incus/expose-buzz.sh enable"
