#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
UV_VERSION="0.12.1"
UV_ARCHIVE="uv-x86_64-unknown-linux-gnu.tar.gz"
UV_ARCHIVE_SHA256="90b2f223fb69d19db49e117da601f64978593417988530aa733d456141b4bcbb"
NODE_VERSION="22.23.2"
NODE_SHA256="b294a556e639d64338823920e5866c21c02741742d2e1529ee1a225c1ec9252a"
PI_VERSION="0.83.0"

apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  git

curl -LsSf -o /tmp/node.tar.gz \
  "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-x64.tar.gz"
printf '%s  %s\n' "$NODE_SHA256" /tmp/node.tar.gz | sha256sum --check --strict
mkdir -p /opt/node && tar --strip-components=1 -xzf /tmp/node.tar.gz -C /opt/node
ln -sf /opt/node/bin/{node,npm,npx} /usr/local/bin/

npm cache clean --force
CODEX_VERSION="$(npm view @openai/codex@latest version)"
CODEX_NPM_INTEGRITY="$(npm view "@openai/codex@$CODEX_VERSION" dist.integrity)"
npm install -g "@openai/codex@$CODEX_VERSION"
PI_NPM_INTEGRITY="$(npm view "@earendil-works/pi-coding-agent@$PI_VERSION" dist.integrity)"
npm install -g "@earendil-works/pi-coding-agent@$PI_VERSION"
ln -sf /opt/node/bin/{codex,pi} /usr/local/bin/

curl -LsSf -o "/tmp/$UV_ARCHIVE" \
  "https://github.com/astral-sh/uv/releases/download/$UV_VERSION/$UV_ARCHIVE"
printf '%s  %s\n' "$UV_ARCHIVE_SHA256" "/tmp/$UV_ARCHIVE" | sha256sum --check --strict
tar -xzf "/tmp/$UV_ARCHIVE" -C /tmp
install -m 0755 /tmp/uv-x86_64-unknown-linux-gnu/uv /usr/local/bin/uv
rm -rf -- "/tmp/$UV_ARCHIVE" /tmp/uv-x86_64-unknown-linux-gnu

codex --version
pi --version
git --version
node --version
uv --version

install -d -m 0755 /usr/local/share/dorf
printf '%s\n' \
  '{' \
  '  "package": "@openai/codex",' \
  "  \"version\": \"$CODEX_VERSION\"," \
  "  \"npm_integrity\": \"$CODEX_NPM_INTEGRITY\"," \
  "  \"pi_version\": \"$PI_VERSION\"," \
  "  \"pi_npm_integrity\": \"$PI_NPM_INTEGRITY\"," \
  "  \"tools\": {\"git\": \"$(git --version | cut -d' ' -f3)\", \"node\": \"$(node --version)\", \"pi\": \"$(pi --version)\", \"uv\": \"$(uv --version | cut -d' ' -f2)\"}," \
  "  \"tool_integrity\": {\"uv\": \"sha256:$UV_ARCHIVE_SHA256\"}" \
  '}' \
  > /usr/local/share/dorf/image.json
chmod 0644 /usr/local/share/dorf/image.json

apt-get clean
rm -rf /var/lib/apt/lists/* /root/.cache /root/.npm /tmp/node.tar.gz
rm -f /root/.bash_history
truncate -s 0 /etc/machine-id || true
rm -f /var/lib/dbus/machine-id
