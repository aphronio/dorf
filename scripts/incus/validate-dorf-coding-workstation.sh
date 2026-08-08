#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 8 ]]; then
  echo "usage: $0 IMAGE FINGERPRINT PROVIDER SOURCE_COMMIT PROOF_ID EVIDENCE_DIR NETWORK DISK_SIZE" >&2
  exit 2
fi
IMAGE="$1"
FINGERPRINT="$2"
PROVIDER="$3"
SOURCE_COMMIT="$4"
PROOF_ID="$5"
EVIDENCE_DIR="$6"
NETWORK="$7"
DISK_SIZE="$8"
PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"

for command in go git jq; do
  command -v "$command" >/dev/null || { echo "missing release proof command: $command" >&2; exit 1; }
done
if [[ -z "${GITHUB_INSTALLATION_ID:-}" ]]; then
  echo "Set GITHUB_INSTALLATION_ID to the Dorf GitHub App installation used by the real proof." >&2
  exit 2
fi
if [[ "$(git -C "$PROJECT_ROOT" rev-parse HEAD)" != "$SOURCE_COMMIT" ]] || [[ -n "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=all)" ]]; then
  echo "Release validation requires the exact clean source commit." >&2
  exit 1
fi

PROOF_ROOT="$(mktemp -d)"
BINARY="$PROOF_ROOT/dorf"
GOAL_FILE="$PROOF_ROOT/goal.txt"
ADMISSION_KEY="image-proof:$PROOF_ID:$FINGERPRINT"
JOB_ID=""
cleanup() {
  if [[ -n "$JOB_ID" ]]; then
    "$BINARY" cleanup "$JOB_ID" >/dev/null 2>&1 || "$BINARY" cleanup --now "$JOB_ID" >/dev/null 2>&1 || true
    "$BINARY" worker --once >/dev/null 2>&1 || true
  fi
  rm -rf -- "$PROOF_ROOT"
}
trap cleanup EXIT

go -C "$PROJECT_ROOT" build -o "$BINARY" ./cmd/dorf
export DORF_INCUS_IMAGE="$IMAGE"
export DORF_INCUS_NETWORK="$NETWORK"
export DORF_INCUS_DISK_SIZE="$DISK_SIZE"
"$BINARY" doctor --provider "$PROVIDER"
printf '%s\n' \
  'Inspect the cloned repository without modifying it. Report the exact Git Revision and installed Codex, Git, Go, Node, and uv versions. Keep the response concise.' \
  >"$GOAL_FILE"

ADMISSION="$($BINARY admit \
  --key "$ADMISSION_KEY" \
  --goal-file "$GOAL_FILE" \
  --repo https://github.com/aphronio/dorf.git \
  --revision "$SOURCE_COMMIT" \
  --branch "dorf/image-proof-$PROOF_ID" \
  --github-repo aphronio/dorf \
  --github-installation "$GITHUB_INSTALLATION_ID" \
  --base "${BASE_BRANCH:-main}" \
  --provider "$PROVIDER" \
  --model gpt-5.6-sol \
  --reasoning low)"
JOB_ID="$(jq -er .job_id <<<"$ADMISSION")"
"$BINARY" worker --once
INSPECTION="$($BINARY inspect --json "$JOB_ID")"
jq -e '.observed_facts.actions | any(.kind == "repository-setup" and .state == "succeeded")' <<<"$INSPECTION" >/dev/null
jq -e '.claims.implementation_agent_runs | map(select(.sequence == 1 and .native_outcome == "completed" and (.native_turn_id | length > 0))) | length == 1' <<<"$INSPECTION" >/dev/null

"$BINARY" cleanup "$JOB_ID"
"$BINARY" worker --once
INSPECTION="$($BINARY inspect --json "$JOB_ID")"
jq -e '.job.cleanup_state == "complete"' <<<"$INSPECTION" >/dev/null
mkdir -p "$EVIDENCE_DIR"
jq -n \
  --arg image "$IMAGE" \
  --arg fingerprint "$FINGERPRINT" \
  --arg source "$SOURCE_COMMIT" \
  --arg provider "$PROVIDER" \
  --arg job "$JOB_ID" \
  '{schema_version:3,image:{alias:$image,fingerprint:$fingerprint},source_commit:$source,provider_connection:$provider,job_id:$job,execution:"Go durable Job spine",cleanup_state:"complete"}' \
  >"$EVIDENCE_DIR/terminal.json"
printf 'Candidate Go terminal passed: %s\n' "$JOB_ID"
JOB_ID=""
