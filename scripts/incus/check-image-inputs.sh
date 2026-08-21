#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly DESCRIPTOR="$PROJECT_ROOT/internal/release/official_image.json"
readonly INPUTS=(
  scripts/incus/build-dorf-image.sh
  scripts/sandbox/provision-dorf-guest.sh
)

for command in git go jq sha256sum; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required image-input check command is unavailable: $command" >&2
    exit 1
  fi
done

release_tag="$(jq -er '.release_tag | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))' "$DESCRIPTOR")"
product_version="$(go -C "$PROJECT_ROOT" run ./cmd/dorf version | awk '{print $2}')"
if [[ "$release_tag" == "v$product_version" ]]; then
  echo "Official Incus image promotion selected for $release_tag."
  exit 0
fi

if ! git -C "$PROJECT_ROOT" rev-parse --verify --quiet "$release_tag^{commit}" >/dev/null; then
  echo "Official Incus image tag is unavailable locally: $release_tag; fetch immutable release tags, then rerun the check." >&2
  exit 1
fi

for path in "${INPUTS[@]}"; do
  current="$(sha256sum "$PROJECT_ROOT/$path" | awk '{print $1}')"
  promoted="$(git -C "$PROJECT_ROOT" show "$release_tag:$path" | sha256sum | awk '{print $1}')"
  if [[ "$current" != "$promoted" ]]; then
    printf '%s\n' \
      "Official Incus image input changed since $release_tag: $path" \
      "Advance the official image release pin to v$product_version to build and prove a new image." >&2
    exit 1
  fi
done

echo "Official Incus image inputs unchanged since $release_tag."
