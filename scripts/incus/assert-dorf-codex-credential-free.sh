#!/usr/bin/env bash
set -euo pipefail

IMAGE_HOME="${DORF_IMAGE_HOME:-/root}"

for path in \
  "$IMAGE_HOME/.codex/auth.json" \
  "$IMAGE_HOME/.codex/config.toml" \
  "$IMAGE_HOME/.config/dorf/provider-route.key" \
  "$IMAGE_HOME/.config/gh/hosts.yml" \
  "$IMAGE_HOME/.dorf-git-credentials" \
  "$IMAGE_HOME/.git-credentials" \
  "$IMAGE_HOME/.netrc" \
  "$IMAGE_HOME/.factory" \
  "$IMAGE_HOME/.config/factory" \
  "$IMAGE_HOME/.local/share/factory" \
  "$IMAGE_HOME/.ssh/id_rsa" \
  "$IMAGE_HOME/.ssh/id_ed25519" \
  "$IMAGE_HOME/.aws/credentials" \
  "$IMAGE_HOME/.config/gcloud/credentials.db"; do
  test ! -e "$path" || {
    echo "Forbidden owner credential path in image: $path" >&2
    exit 1
  }
done

for variable in \
  OPENAI_API_KEY \
  DEEPSEEK_API_KEY \
  GITHUB_TOKEN \
  GH_TOKEN \
  FACTORY_API_KEY \
  AWS_ACCESS_KEY_ID \
  GOOGLE_APPLICATION_CREDENTIALS; do
  test -z "${!variable:-}" || {
    echo "Forbidden owner credential variable in image: $variable" >&2
    exit 1
  }
done
