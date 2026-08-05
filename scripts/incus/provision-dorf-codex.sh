#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  git \
  nodejs \
  npm

npm cache clean --force
CODEX_VERSION="$(npm view @openai/codex@latest version)"
CODEX_NPM_INTEGRITY="$(npm view "@openai/codex@$CODEX_VERSION" dist.integrity)"
npm install -g "@openai/codex@$CODEX_VERSION"

curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin sh

apt-get purge -y npm
apt-get autoremove -y --purge

codex --version
git --version
node --version
uv --version

install -d -m 0755 /usr/local/share/dorf
printf '%s\n' \
  '{' \
  '  "package": "@openai/codex",' \
  "  \"version\": \"$CODEX_VERSION\"," \
  "  \"npm_integrity\": \"$CODEX_NPM_INTEGRITY\"," \
  "  \"tools\": {\"git\": \"$(git --version | cut -d' ' -f3)\", \"node\": \"$(node --version)\", \"uv\": \"$(uv --version | cut -d' ' -f2)\"}" \
  '}' \
  > /usr/local/share/dorf/image.json
chmod 0644 /usr/local/share/dorf/image.json

apt-get clean
rm -rf /var/lib/apt/lists/* /root/.cache /root/.npm
rm -f /root/.bash_history
truncate -s 0 /etc/machine-id || true
rm -f /var/lib/dbus/machine-id
