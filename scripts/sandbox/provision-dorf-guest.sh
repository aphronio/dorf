#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

readonly PYTHON_VERSION="3.14.4"
readonly UV_VERSION="0.12.3"
readonly UV_ARCHIVE="uv-x86_64-unknown-linux-gnu.tar.gz"
readonly UV_ARCHIVE_SHA256="600cf9a742aca00d292673b16b5acffaa7b8c269a364ad0c2e79498dcb1fe101"
readonly NODE_VERSION="24.19.0"
readonly NODE_ARCHIVE="node-v$NODE_VERSION-linux-x64.tar.xz"
readonly NODE_SHA256="14b342e71204f811bde6153be8e04b62aef63c236fef92b55f9c83154b409647"
readonly GO_VERSION="1.26.5"
readonly GO_SHA256="5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053"
readonly CODEX_PACKAGE="@openai/codex"
readonly CODEX_VERSION="0.147.0"
readonly CODEX_NPM_INTEGRITY="sha512-EQLEXecAG2ptxI7UpBMo2TR/ga5596/c/OsYF/0LoUDh5JANZ7IoGqlzBEWbuEVQ76JePIbtTW/ihCkp1a7Z3w=="
readonly PI_PACKAGE="@earendil-works/pi-coding-agent"
readonly PI_VERSION="0.84.1"
readonly PI_NPM_INTEGRITY="sha512-ncAqFrG+iybuPGOhMiZoEHkEzTpJgz3guYD32pD+M7ucc0WeHmauP6wa7qwP8V/KWvsZDVNa5XGsdZ7fkC7w7A=="

source /etc/os-release
if [[ "${ID:-}" != "debian" || "${VERSION_ID:-}" != "13" ]]; then
  echo "Dorf's supported Sandbox profile requires Debian 13; observed ${ID:-unknown} ${VERSION_ID:-unknown}." >&2
  exit 1
fi
if [[ -z "${DORF_BASE_IMAGE:-}" ]] ||
  [[ ! "${DORF_BASE_FINGERPRINT:-}" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Dorf's supported Sandbox profile requires an exact Debian 13 base reference and identity." >&2
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

observed_codex_integrity="$(npm view "$CODEX_PACKAGE@$CODEX_VERSION" dist.integrity)"
observed_pi_integrity="$(npm view "$PI_PACKAGE@$PI_VERSION" dist.integrity)"
if [[ "$observed_codex_integrity" != "$CODEX_NPM_INTEGRITY" ]] ||
  [[ "$observed_pi_integrity" != "$PI_NPM_INTEGRITY" ]]; then
  echo "A pinned Harness package no longer matches its recorded npm integrity." >&2
  exit 1
fi
npm install -g "$CODEX_PACKAGE@$CODEX_VERSION" "$PI_PACKAGE@$PI_VERSION"
ln -sf /opt/node/bin/codex /usr/local/bin/codex
ln -sf /opt/node/bin/pi /usr/local/bin/pi
codex --version >/dev/null
pi --version >/dev/null
npm cache clean --force
rm -f /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack \
  /opt/node/bin/npm /opt/node/bin/npx /opt/node/bin/corepack \
  /usr/local/bin/yarn /usr/local/bin/yarnpkg /usr/local/bin/pnpm /usr/local/bin/pnpx \
  /opt/node/bin/yarn /opt/node/bin/yarnpkg /opt/node/bin/pnpm /opt/node/bin/pnpx
rm -rf -- /opt/node/lib/node_modules/npm /opt/node/lib/node_modules/corepack /root/.npm

install -d -m 0755 /usr/local/share/dorf /workspace/job
jq -n \
  --arg codex_package "$CODEX_PACKAGE" \
  --arg codex_version "$CODEX_VERSION" \
  --arg codex_npm_integrity "$CODEX_NPM_INTEGRITY" \
  --arg pi_package "$PI_PACKAGE" \
  --arg pi_version "$PI_VERSION" \
  --arg pi_npm_integrity "$PI_NPM_INTEGRITY" \
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
    harnesses: {
      codex: {package: $codex_package, version: $codex_version, npm_integrity: $codex_npm_integrity},
      pi: {package: $pi_package, version: $pi_version, npm_integrity: $pi_npm_integrity}
    },
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
