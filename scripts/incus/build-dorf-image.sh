#!/usr/bin/env bash
set -euo pipefail

readonly HARNESS="${DORF_HARNESS:-codex}"
if [[ "$HARNESS" != "codex" && "$HARNESS" != "pi" ]]; then
  echo "DORF_HARNESS must be codex or pi." >&2
  exit 2
fi

provision() {
  export DEBIAN_FRONTEND=noninteractive
  readonly EXPECTED_BASE_IMAGE="images:debian/13"
  readonly PYTHON_VERSION="3.14.4"
  readonly UV_VERSION="0.12.3"
  readonly UV_ARCHIVE="uv-x86_64-unknown-linux-gnu.tar.gz"
  readonly UV_ARCHIVE_SHA256="600cf9a742aca00d292673b16b5acffaa7b8c269a364ad0c2e79498dcb1fe101"
  readonly NODE_VERSION="24.19.0"
  readonly NODE_ARCHIVE="node-v$NODE_VERSION-linux-x64.tar.xz"
  readonly NODE_SHA256="14b342e71204f811bde6153be8e04b62aef63c236fef92b55f9c83154b409647"
  readonly GO_VERSION="1.26.5"
  readonly GO_SHA256="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"

  source /etc/os-release
  if [[ "${ID:-}" != "debian" || "${VERSION_ID:-}" != "13" ]]; then
    echo "Dorf's official Sandbox image requires Debian 13; observed ${ID:-unknown} ${VERSION_ID:-unknown}." >&2
    exit 1
  fi
  if [[ "${DORF_BASE_IMAGE:-}" != "$EXPECTED_BASE_IMAGE" ]] ||
    [[ ! "${DORF_BASE_FINGERPRINT:-}" =~ ^[0-9a-f]{64}$ ]]; then
    echo "Dorf's official Sandbox image requires an exact Debian 13 base reference and fingerprint." >&2
    exit 1
  fi

  apt-get update
  apt-get install -y --no-install-recommends \
    bash \
    build-essential \
    ca-certificates \
    curl \
    git \
    jq \
    pkg-config \
    ripgrep \
    tar \
    unzip \
    wget \
    xz-utils

  curl -LsSf -o "/tmp/$NODE_ARCHIVE" \
    "https://nodejs.org/dist/v$NODE_VERSION/$NODE_ARCHIVE"
  printf '%s  %s\n' "$NODE_SHA256" "/tmp/$NODE_ARCHIVE" | sha256sum --check --strict
  mkdir -p /opt/node
  tar --strip-components=1 -xJf "/tmp/$NODE_ARCHIVE" -C /opt/node
  ln -sf /opt/node/bin/{node,npm,npx} /usr/local/bin/

  curl -LsSf -o /tmp/go.tar.gz \
    "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
  printf '%s  %s\n' "$GO_SHA256" /tmp/go.tar.gz | sha256sum --check --strict
  rm -rf -- /usr/local/go
  tar -xzf /tmp/go.tar.gz -C /usr/local
  ln -sf /usr/local/go/bin/{go,gofmt} /usr/local/bin/

  curl -LsSf -o "/tmp/$UV_ARCHIVE" \
    "https://github.com/astral-sh/uv/releases/download/$UV_VERSION/$UV_ARCHIVE"
  printf '%s  %s\n' "$UV_ARCHIVE_SHA256" "/tmp/$UV_ARCHIVE" | sha256sum --check --strict
  tar -xzf "/tmp/$UV_ARCHIVE" -C /tmp
  install -m 0755 /tmp/uv-x86_64-unknown-linux-gnu/uv /usr/local/bin/uv

  export UV_PYTHON_INSTALL_DIR=/opt/uv-python
  uv venv --python "$PYTHON_VERSION" --seed /opt/dorf-python
  ln -sf /opt/dorf-python/bin/python /usr/local/bin/python
  ln -sf /opt/dorf-python/bin/python /usr/local/bin/python3
  ln -sf /opt/dorf-python/bin/pip /usr/local/bin/pip
  ln -sf /opt/dorf-python/bin/pip /usr/local/bin/pip3

  case "$HARNESS" in
    codex)
      HARNESS_PACKAGE="@openai/codex"
      HARNESS_COMMAND="codex"
      ;;
    pi)
      HARNESS_PACKAGE="@earendil-works/pi-coding-agent"
      HARNESS_COMMAND="pi"
      ;;
  esac
  HARNESS_VERSION="$(npm view "$HARNESS_PACKAGE@latest" version)"
  HARNESS_NPM_INTEGRITY="$(npm view "$HARNESS_PACKAGE@$HARNESS_VERSION" dist.integrity)"
  npm install -g "$HARNESS_PACKAGE@$HARNESS_VERSION"
  ln -sf "/opt/node/bin/$HARNESS_COMMAND" "/usr/local/bin/$HARNESS_COMMAND"
  npm cache clean --force
  rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
    /opt/node/bin/npm /opt/node/bin/npx /opt/node/bin/corepack \
    /usr/local/bin/yarn /usr/local/bin/yarnpkg /usr/local/bin/pnpm /usr/local/bin/pnpx \
    /opt/node/bin/yarn /opt/node/bin/yarnpkg /opt/node/bin/pnpm /opt/node/bin/pnpx
  rm -rf -- /opt/node/lib/node_modules/npm /opt/node/lib/node_modules/corepack /root/.npm

  install -d -m 0755 /usr/local/share/dorf
  jq -n \
    --arg harness "$HARNESS" \
    --arg package "$HARNESS_PACKAGE" \
    --arg version "$HARNESS_VERSION" \
    --arg npm_integrity "$HARNESS_NPM_INTEGRITY" \
    --arg base_reference "$DORF_BASE_IMAGE" \
    --arg base_fingerprint "$DORF_BASE_FINGERPRINT" \
    --arg bash "$BASH_VERSION" \
    --arg curl "$(curl --version | sed -n '1{s/^curl \([^ ]*\).*/\1/p;}')" \
    --arg gxx "$(g++ -dumpfullversion)" \
    --arg gcc "$(gcc -dumpfullversion)" \
    --arg git "$(git --version | awk '{print $3}')" \
    --arg go "$(go version | awk '{print $3}')" \
    --arg jq "$(jq --version)" \
    --arg make "$(make --version | sed -n '1{s/^GNU Make //p;}')" \
    --arg node "$(node --version)" \
    --arg pip "$(pip --version | awk '{print $2}')" \
    --arg pkg_config "$(pkg-config --version)" \
    --arg python "$(python --version | awk '{print $2}')" \
    --arg ripgrep "$(rg --version | sed -n '1{s/^ripgrep //p;}')" \
    --arg tar "$(tar --version | sed -n '1{s/^tar (GNU tar) //p;}')" \
    --arg unzip "$(unzip -v | sed -n '1{s/^UnZip \([^ ]*\).*/\1/p;}')" \
    --arg uv "$(uv --version | awk '{print $2}')" \
    --arg wget "$(wget --version | sed -n '1{s/^GNU Wget \([^ ]*\).*/\1/p;}')" \
    --arg go_integrity "sha256:$GO_SHA256" \
    --arg node_integrity "sha256:$NODE_SHA256" \
    --arg uv_integrity "sha256:$UV_ARCHIVE_SHA256" \
    '{
      harness: $harness,
      package: $package,
      version: $version,
      npm_integrity: $npm_integrity,
      base_image: {reference: $base_reference, fingerprint: $base_fingerprint},
      tools: {
        bash: $bash,
        curl: $curl,
        "g++": $gxx,
        gcc: $gcc,
        git: $git,
        go: $go,
        jq: $jq,
        make: $make,
        node: $node,
        pip: $pip,
        "pkg-config": $pkg_config,
        python: $python,
        ripgrep: $ripgrep,
        tar: $tar,
        unzip: $unzip,
        uv: $uv,
        wget: $wget
      },
      tool_integrity: {go: $go_integrity, node: $node_integrity, uv: $uv_integrity}
    }' > /usr/local/share/dorf/image.json
  chmod 0644 /usr/local/share/dorf/image.json

  apt-get clean
  rm -rf -- \
    /var/lib/apt/lists/* \
    /root/.cache \
    "/tmp/$NODE_ARCHIVE" \
    /tmp/go.tar.gz \
    "/tmp/$UV_ARCHIVE" \
    /tmp/uv-x86_64-unknown-linux-gnu
  rm -f /root/.bash_history
  truncate -s 0 /etc/machine-id || true
  rm -f /var/lib/dbus/machine-id
}

if [[ "${1:-}" == "provision" ]]; then
  provision
  exit
fi

readonly IMAGE_ALIAS="${IMAGE_ALIAS:-dorf-$HARNESS}"
readonly BUILD_VM="${BUILD_VM:-dorf-$HARNESS-build}"
readonly BASE_IMAGE="images:debian/13"
readonly NETWORK="${NETWORK:-incusbr0}"
readonly ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
readonly IMAGE_METADATA_PATH="${IMAGE_METADATA_PATH:-}"
readonly IMAGE_SCRIPT="$(realpath -- "$0")"

cleanup() {
  if incus info "$BUILD_VM" >/dev/null 2>&1; then
    incus delete "$BUILD_VM" --force >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if incus info "$BUILD_VM" >/dev/null 2>&1; then
  echo "Build VM already exists: $BUILD_VM" >&2
  exit 1
fi

BASE_FINGERPRINT="$(incus image info "$BASE_IMAGE" --vm | sed -n 's/^Fingerprint: //p')"
if [[ ! "$BASE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Could not resolve an immutable VM fingerprint for $BASE_IMAGE" >&2
  exit 1
fi

incus init "images:$BASE_FINGERPRINT" "$BUILD_VM" \
  --vm --network "$NETWORK" -d "root,size=$ROOT_DISK_SIZE"
incus start "$BUILD_VM"
for _ in {1..60}; do
  if incus exec "$BUILD_VM" -- true >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
incus exec "$BUILD_VM" -- true >/dev/null
incus file push "$IMAGE_SCRIPT" "$BUILD_VM/tmp/build-dorf-image.sh"
incus exec "$BUILD_VM" -- chmod +x /tmp/build-dorf-image.sh
incus exec "$BUILD_VM" -- env \
  "DORF_HARNESS=$HARNESS" \
  "DORF_BASE_IMAGE=$BASE_IMAGE" \
  "DORF_BASE_FINGERPRINT=$BASE_FINGERPRINT" \
  /tmp/build-dorf-image.sh provision
incus exec "$BUILD_VM" -- rm -f /tmp/build-dorf-image.sh

HARNESS_VERSION="$(incus exec "$BUILD_VM" -- jq -r .version /usr/local/share/dorf/image.json)"
if [[ -n "$IMAGE_METADATA_PATH" ]]; then
  mkdir -p "$(dirname -- "$IMAGE_METADATA_PATH")"
  incus file pull "$BUILD_VM/usr/local/share/dorf/image.json" "$IMAGE_METADATA_PATH"
fi
incus exec "$BUILD_VM" -- sync
incus stop "$BUILD_VM" --timeout 60
incus publish "$BUILD_VM" --alias "$IMAGE_ALIAS" --reuse \
  description="Dorf Debian 13 VM with the $HARNESS $HARNESS_VERSION harness" \
  "dorf.$HARNESS.version=$HARNESS_VERSION" \
  dorf.source.base_fingerprint="$BASE_FINGERPRINT"

echo "Published local Incus image alias: $IMAGE_ALIAS"
