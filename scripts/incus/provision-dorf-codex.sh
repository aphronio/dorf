#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

apt-get update
apt-get install -y --no-install-recommends \
  ca-certificates \
  nodejs \
  npm

npm cache clean --force
CODEX_VERSION="$(npm view @openai/codex@latest version)"
CODEX_NPM_INTEGRITY="$(npm view "@openai/codex@$CODEX_VERSION" dist.integrity)"
npm install -g "@openai/codex@$CODEX_VERSION"

apt-get purge -y npm
apt-get autoremove -y --purge

codex --version
node --version

install -d -m 0755 /usr/local/share/dorf
printf '%s\n' \
  '{' \
  '  "package": "@openai/codex",' \
  "  \"version\": \"$CODEX_VERSION\"," \
  "  \"npm_integrity\": \"$CODEX_NPM_INTEGRITY\"" \
  '}' \
  > /usr/local/share/dorf/image.json
chmod 0644 /usr/local/share/dorf/image.json

apt-get clean
rm -rf /var/lib/apt/lists/* /root/.cache /root/.npm
rm -f /root/.bash_history
truncate -s 0 /etc/machine-id || true
rm -f /var/lib/dbus/machine-id
