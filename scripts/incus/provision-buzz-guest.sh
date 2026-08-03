#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

BUZZ_IMAGE="${BUZZ_IMAGE:-ghcr.io/block/buzz:sha-2ce2d71}"
BUZZ_REPOSITORY="${BUZZ_REPOSITORY:-https://github.com/block/buzz.git}"
BUZZ_REVISION="${BUZZ_REVISION:-2ce2d71cc38a9657eaf344c10e07f155b8a18615}"
BUZZ_SOURCE_DIR="${BUZZ_SOURCE_DIR:-/opt/dorf-buzz/source}"
BUZZ_INITIAL_DOMAIN="${BUZZ_INITIAL_DOMAIN:-dorf-buzz.local}"
BUZZ_OWNER_PUBKEY="${BUZZ_OWNER_PUBKEY:-}"
COMPOSE_DIR="$BUZZ_SOURCE_DIR/deploy/compose"
ENV_FILE="$COMPOSE_DIR/.env"

install_packages() {
  apt-get update
  apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    gnupg \
    jq \
    openssl

  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
    -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc

  local architecture
  local codename
  architecture="$(dpkg --print-architecture)"
  codename="$(. /etc/os-release && printf '%s' "$VERSION_CODENAME")"
  printf '%s\n' \
    "deb [arch=$architecture signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu $codename stable" \
    > /etc/apt/sources.list.d/docker.list

  apt-get update
  apt-get install -y --no-install-recommends \
    containerd.io \
    docker-buildx-plugin \
    docker-ce \
    docker-ce-cli \
    docker-compose-plugin

  systemctl enable --now docker
}

checkout_buzz() {
  install -d -m 0755 "$(dirname "$BUZZ_SOURCE_DIR")"

  if [[ ! -d "$BUZZ_SOURCE_DIR/.git" ]]; then
    git clone --filter=blob:none --no-checkout "$BUZZ_REPOSITORY" "$BUZZ_SOURCE_DIR"
  fi

  git -C "$BUZZ_SOURCE_DIR" fetch --depth 1 origin "$BUZZ_REVISION"
  git -C "$BUZZ_SOURCE_DIR" checkout --detach --force FETCH_HEAD

  local actual_revision
  actual_revision="$(git -C "$BUZZ_SOURCE_DIR" rev-parse HEAD)"
  if [[ "$actual_revision" != "$BUZZ_REVISION" ]]; then
    echo "Buzz checkout mismatch: expected $BUZZ_REVISION, got $actual_revision" >&2
    exit 1
  fi
}

keypair() {
  docker run --rm \
    --entrypoint /usr/local/bin/buzz-admin \
    "$BUZZ_IMAGE" \
    generate-key
}

secret_hex() {
  openssl rand -hex "${1:-32}"
}

write_initial_environment() {
  if [[ -f "$ENV_FILE" ]]; then
    return
  fi

  local relay_keypair
  local relay_private_key

  if [[ ! "$BUZZ_OWNER_PUBKEY" =~ ^[0-9a-f]{64}$ ]]; then
    echo "A normalized BUZZ_OWNER_PUBKEY is required before creating $ENV_FILE" >&2
    exit 1
  fi

  relay_keypair="$(keypair)"
  relay_private_key="$(awk '/^Secret key:/ {print $3}' <<<"$relay_keypair")"

  if [[ ! "$relay_private_key" =~ ^[0-9a-f]{64}$ ]]; then
    echo "buzz-admin returned an invalid relay keypair" >&2
    exit 1
  fi

  umask 077
  cat > "$ENV_FILE" <<EOF
BUZZ_IMAGE=$BUZZ_IMAGE
BUZZ_DOMAIN=$BUZZ_INITIAL_DOMAIN
RELAY_URL=ws://127.0.0.1:3000
BUZZ_MEDIA_BASE_URL=http://127.0.0.1:3000/media
BUZZ_MEDIA_SERVER_DOMAIN=$BUZZ_INITIAL_DOMAIN
BUZZ_CORS_ORIGINS=http://127.0.0.1:3000
BUZZ_REQUIRE_AUTH_TOKEN=true
BUZZ_REQUIRE_RELAY_MEMBERSHIP=true
BUZZ_ALLOW_NIP_OA_AUTH=true
BUZZ_AUTO_MIGRATE=true
BUZZ_GIT_CONFORMANCE_PROBE=true
RUST_LOG=buzz_relay=info,buzz_db=info,buzz_auth=info,buzz_pubsub=info,tower_http=info
RELAY_OWNER_PUBKEY=$BUZZ_OWNER_PUBKEY
BUZZ_RELAY_PRIVATE_KEY=$relay_private_key
BUZZ_GIT_HOOK_HMAC_SECRET=$(secret_hex 32)
POSTGRES_DB=buzz
POSTGRES_USER=buzz
POSTGRES_PASSWORD=$(secret_hex 32)
REDIS_PASSWORD=$(secret_hex 32)
TYPESENSE_API_KEY=$(secret_hex 32)
BUZZ_S3_ACCESS_KEY=$(secret_hex 16)
BUZZ_S3_SECRET_KEY=$(secret_hex 32)
BUZZ_S3_BUCKET=buzz-media
BUZZ_HTTP_PORT=127.0.0.1:3000
CADDY_HTTP_PORT=80
CADDY_HTTPS_PORT=443
EOF
  chmod 0600 "$ENV_FILE"
}

validate_environment() {
  if grep -Eq '^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*=.*CHANGE_ME' "$ENV_FILE"; then
    echo "Buzz environment contains a CHANGE_ME placeholder" >&2
    exit 1
  fi

  local configured_image
  local configured_owner_public_key
  configured_image="$(sed -n 's/^BUZZ_IMAGE=//p' "$ENV_FILE")"
  configured_owner_public_key="$(sed -n 's/^RELAY_OWNER_PUBKEY=//p' "$ENV_FILE")"
  if [[ "$configured_image" != "$BUZZ_IMAGE" ]]; then
    echo "Buzz image mismatch: expected $BUZZ_IMAGE, found $configured_image" >&2
    exit 1
  fi

  if [[ ! "$configured_owner_public_key" =~ ^[0-9a-f]{64}$ ]]; then
    echo "Buzz environment contains an invalid RELAY_OWNER_PUBKEY" >&2
    exit 1
  fi

  if [[ -n "$BUZZ_OWNER_PUBKEY" ]] &&
    [[ "$configured_owner_public_key" != "$BUZZ_OWNER_PUBKEY" ]]; then
    cat >&2 <<EOF
Refusing to replace the configured Buzz owner during provisioning.
Configured owner: $configured_owner_public_key
Requested owner:  $BUZZ_OWNER_PUBKEY
Follow the documented owner recovery procedure for an intentional rotation.
EOF
    exit 1
  fi
}

start_buzz() {
  cd "$COMPOSE_DIR"
  ./run.sh config >/dev/null
  ./run.sh start
  curl --fail --silent --show-error http://127.0.0.1:3000/_liveness
  printf '\n'
  ./run.sh status
}

main() {
  install_packages
  docker compose version
  checkout_buzz
  docker pull "$BUZZ_IMAGE"
  write_initial_environment
  validate_environment
  start_buzz

  local configured_owner_public_key
  configured_owner_public_key="$(sed -n 's/^RELAY_OWNER_PUBKEY=//p' "$ENV_FILE")"
  echo "Buzz is healthy on VM loopback port 3000."
  echo "Owner public key: $configured_owner_public_key"
  echo "The human owner private key remains in Buzz Desktop and its human-controlled backup."
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
