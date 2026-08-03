#!/usr/bin/env bash
set -euo pipefail

session_id="${1:?usage: scripts/fake-agent-ready-smoke.sh <session-id>}"
smoke_file="${DORF_FAKE_AGENT_FILE:-dorf-fake-agent-smoke.txt}"
export UV_CACHE_DIR="${UV_CACHE_DIR:-.dorf/uv-cache}"

uv run dorf exec "$session_id" -- bash -lc \
  'printf "fake agent readiness smoke: %s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$1"' \
  _ "$smoke_file"

uv run dorf check "$session_id"
uv run dorf exec "$session_id" -- bash -lc \
  'git config user.email >/dev/null || git config user.email dorf@example.com
   git config user.name >/dev/null || git config user.name "Dorf Fake Agent"
   git add "$1"
   git commit -m "Run fake agent readiness smoke"' \
  _ "$smoke_file"

uv run dorf check "$session_id"
uv run dorf exec "$session_id" -- bash -lc 'git push origin "HEAD:$DORF_SESSION_BRANCH"'
uv run dorf ready "$session_id"
