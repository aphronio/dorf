#!/usr/bin/env bash
set -euo pipefail

BUZZ_VM="${BUZZ_VM:-dorf-buzz}"
BUZZ_HOSTNAME="${BUZZ_HOSTNAME:-omarchy}"
BUZZ_PORT="${BUZZ_PORT:-3000}"
PROXY_DEVICE="${PROXY_DEVICE:-buzz-tailnet}"
COMPOSE_DIR="${COMPOSE_DIR:-/opt/dorf-buzz/source/deploy/compose}"
ENV_FILE="$COMPOSE_DIR/.env"

TAILSCALE_IP="$(tailscale ip -4)"
LISTEN_ADDRESS="tcp:$TAILSCALE_IP:$BUZZ_PORT"
VM_INTERFACE="$(
  incus exec "$BUZZ_VM" -- sh -c \
    'ip -4 route show default | awk "NR == 1 {print \$5}"'
)"
VM_IP="$(
  incus exec "$BUZZ_VM" -- ip -4 -j address show dev "$VM_INTERFACE" |
    jq -r '.[0].addr_info[] | select(.family == "inet" and .scope == "global") | .local'
)"
CONNECT_ADDRESS="tcp:$VM_IP:3000"

set_buzz_urls() {
  local scheme="$1"
  local websocket_scheme="$2"
  local hostname="$3"
  local publish_address="$4"

  incus exec "$BUZZ_VM" \
    --env "BUZZ_ENV_FILE=$ENV_FILE" \
    --env "BUZZ_EXTERNAL_SCHEME=$scheme" \
    --env "BUZZ_EXTERNAL_WS_SCHEME=$websocket_scheme" \
    --env "BUZZ_EXTERNAL_HOST=$hostname" \
    --env "BUZZ_EXTERNAL_PORT=$BUZZ_PORT" \
    --env "BUZZ_PUBLISH_ADDRESS=$publish_address" \
    -- bash -euo pipefail -c '
      sed -i \
        -e "s|^BUZZ_DOMAIN=.*|BUZZ_DOMAIN=$BUZZ_EXTERNAL_HOST|" \
        -e "s|^RELAY_URL=.*|RELAY_URL=$BUZZ_EXTERNAL_WS_SCHEME://$BUZZ_EXTERNAL_HOST:$BUZZ_EXTERNAL_PORT|" \
        -e "s|^BUZZ_MEDIA_BASE_URL=.*|BUZZ_MEDIA_BASE_URL=$BUZZ_EXTERNAL_SCHEME://$BUZZ_EXTERNAL_HOST:$BUZZ_EXTERNAL_PORT/media|" \
        -e "s|^BUZZ_MEDIA_SERVER_DOMAIN=.*|BUZZ_MEDIA_SERVER_DOMAIN=$BUZZ_EXTERNAL_HOST|" \
        -e "s|^BUZZ_CORS_ORIGINS=.*|BUZZ_CORS_ORIGINS=$BUZZ_EXTERNAL_SCHEME://$BUZZ_EXTERNAL_HOST:$BUZZ_EXTERNAL_PORT|" \
        -e "s|^BUZZ_HTTP_PORT=.*|BUZZ_HTTP_PORT=$BUZZ_PUBLISH_ADDRESS|" \
        "$BUZZ_ENV_FILE"
    '
}

restart_relay() {
  incus exec "$BUZZ_VM" --cwd "$COMPOSE_DIR" -- ./run.sh restart
}

device_exists() {
  incus config device get "$BUZZ_VM" "$PROXY_DEVICE" listen >/dev/null 2>&1
}

enable_proxy() {
  incus config device set "$BUZZ_VM" eth0 "ipv4.address=$VM_IP"

  if device_exists; then
    incus config device set "$BUZZ_VM" "$PROXY_DEVICE" \
      "listen=$LISTEN_ADDRESS" \
      "connect=$CONNECT_ADDRESS" \
      "nat=true"
  else
    if ss -H -ltn "( sport = :$BUZZ_PORT )" | grep -q .; then
      echo "Host TCP port $BUZZ_PORT is already in use" >&2
      exit 1
    fi
    incus config device add "$BUZZ_VM" "$PROXY_DEVICE" proxy \
      "listen=$LISTEN_ADDRESS" \
      "connect=$CONNECT_ADDRESS" \
      "nat=true"
  fi
}

show_status() {
  echo "Host mapping:"
  if device_exists; then
    incus config device show "$BUZZ_VM" |
      awk -v header="$PROXY_DEVICE:" '
        $0 == header { printing = 1 }
        printing && $0 != header && $0 ~ /^[^[:space:]]/ { exit }
        printing { print }
      '
  else
    echo "  disabled"
  fi

  echo "Buzz advertised URLs:"
  incus exec "$BUZZ_VM" -- sed -n \
    -e '/^BUZZ_DOMAIN=/p' \
    -e '/^RELAY_URL=/p' \
    -e '/^BUZZ_MEDIA_BASE_URL=/p' \
    -e '/^BUZZ_CORS_ORIGINS=/p' \
    "$ENV_FILE"

  if device_exists; then
    curl --fail --silent --show-error "http://$TAILSCALE_IP:$BUZZ_PORT/_liveness"
    printf '\n'
  fi
}

case "${1:-status}" in
  enable)
    enable_proxy
    set_buzz_urls http ws "$BUZZ_HOSTNAME" "$BUZZ_PORT"
    restart_relay
    show_status
    echo "Buzz: http://$BUZZ_HOSTNAME:$BUZZ_PORT"
    echo "Relay: ws://$BUZZ_HOSTNAME:$BUZZ_PORT"
    echo "Note: production Buzz mobile requires WSS; this endpoint is for relay/desktop validation."
    ;;
  disable)
    if device_exists; then
      incus config device remove "$BUZZ_VM" "$PROXY_DEVICE"
    fi
    incus config device unset "$BUZZ_VM" eth0 ipv4.address
    set_buzz_urls http ws 127.0.0.1 "127.0.0.1:3000"
    restart_relay
    show_status
    ;;
  status)
    show_status
    ;;
  *)
    echo "Usage: $0 {enable|status|disable}" >&2
    exit 1
    ;;
esac
