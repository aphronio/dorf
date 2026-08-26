#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 0 ]]; then
  echo "usage: $0" >&2
  exit 2
fi
if [[ -z "${AI_CONNECTION:-}" ]]; then
  echo "Set AI_CONNECTION to one ready AI connection name." >&2
  exit 2
fi
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly OUTPUT_DIR="${OUTPUT_DIR:-$PROJECT_ROOT/dist/incus-image}"
readonly CANDIDATE_NETWORK="${NETWORK:-incusbr0}"
readonly CANDIDATE_ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
readonly SOURCE_COMMIT="${SOURCE_COMMIT:-$(git -C "$PROJECT_ROOT" rev-parse HEAD)}"
readonly BUILD_ID="$(date -u +%Y%m%d%H%M%S)"
readonly CANDIDATE_ALIAS="dorf-candidate-$BUILD_ID"
readonly CANDIDATE_BUILD_VM="dorf-build-$BUILD_ID"
readonly ARCHIVE_BASENAME="dorf-incus-vm-v5-x86_64"
readonly ARCHIVE_PATH="$OUTPUT_DIR/$ARCHIVE_BASENAME.tar.gz"
readonly MANIFEST_PATH="$OUTPUT_DIR/$ARCHIVE_BASENAME.json"
readonly METADATA_PATH="$OUTPUT_DIR/image.json"
readonly EVIDENCE_DIR="$OUTPUT_DIR/workstation-evidence"
readonly EVIDENCE_POLICY="${EVIDENCE_POLICY:-retain}"
readonly PROOF_ROOT="$(mktemp -d)"
readonly BINARY="$PROOF_ROOT/dorf"
JOB_ID=""

if [[ -z "${DORF_DATABASE_URL:-}" && -n "${DORF_TEST_DATABASE_URL:-}" ]]; then
  DORF_DATABASE_URL="$DORF_TEST_DATABASE_URL"
  export DORF_DATABASE_URL
fi
if [[ -z "${DORF_DATABASE_URL:-}" ]]; then
  echo "Set DORF_DATABASE_URL or run the release through the repository Mise environment." >&2
  exit 2
fi

drive_job_until() {
  local job_id="$1"
  local predicate="$2"
  local description="$3"
  local deadline=$((SECONDS + 3600))
  local inspection=""

  while ((SECONDS < deadline)); do
    "$BINARY" worker --once >/dev/null
    inspection="$("$BINARY" inspect --json "$job_id")"
    if jq -e "$predicate" <<<"$inspection" >/dev/null; then
      printf '%s\n' "$inspection"
      return
    fi
    sleep 1
  done
  echo "Timed out waiting for $description on Job $job_id." >&2
  return 1
}

cleanup() {
  if [[ -n "$JOB_ID" ]]; then
    "$BINARY" cleanup "$JOB_ID" >/dev/null 2>&1 || true
    drive_job_until "$JOB_ID" '.job.cleanup_state == "complete"' "cleanup" >/dev/null 2>&1 || true
  fi
  if [[ "$EVIDENCE_POLICY" == "remove" ]]; then
    rm -rf -- "$EVIDENCE_DIR"
  fi
  if command -v incus >/dev/null 2>&1 && incus image info "$CANDIDATE_ALIAS" >/dev/null 2>&1; then
    incus image delete "$CANDIDATE_ALIAS" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$PROOF_ROOT"
}
trap cleanup EXIT

if [[ "$EVIDENCE_POLICY" != "retain" && "$EVIDENCE_POLICY" != "remove" ]]; then
  echo "EVIDENCE_POLICY must be retain or remove." >&2
  exit 2
fi
if [[ "$(git -C "$PROJECT_ROOT" rev-parse HEAD)" != "$SOURCE_COMMIT" ]] ||
  [[ -n "$(git -C "$PROJECT_ROOT" status --porcelain --untracked-files=all)" ]]; then
  echo "Release validation requires the exact clean source commit." >&2
  exit 1
fi

for command in go incus git jq; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required release command is unavailable: $command" >&2
    exit 1
  fi
done

"$SCRIPT_DIR/check-image-inputs.sh"
PRODUCT_VERSION="$(go -C "$PROJECT_ROOT" run ./cmd/dorf version | awk '{print $2}')"
RELEASE_TAG="${RELEASE_TAG:-v$PRODUCT_VERSION}"
OFFICIAL_IMAGE_RELEASE="$(jq -er .release_tag "$PROJECT_ROOT/internal/release/official_image.json")"
if [[ "$RELEASE_TAG" != "v$PRODUCT_VERSION" ]] || [[ "$OFFICIAL_IMAGE_RELEASE" != "$RELEASE_TAG" ]]; then
  echo "Incus image promotion requires the application release and official image pin to agree: application v$PRODUCT_VERSION, image $OFFICIAL_IMAGE_RELEASE, requested $RELEASE_TAG." >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"
rm -f "$METADATA_PATH" "$ARCHIVE_PATH" "$MANIFEST_PATH"
go -C "$PROJECT_ROOT" build -o "$BINARY" ./cmd/dorf

IMAGE_ALIAS="$CANDIDATE_ALIAS" \
BUILD_VM="$CANDIDATE_BUILD_VM" \
NETWORK="$CANDIDATE_NETWORK" \
ROOT_DISK_SIZE="$CANDIDATE_ROOT_DISK_SIZE" \
IMAGE_METADATA_PATH="$METADATA_PATH" \
  "$SCRIPT_DIR/build-dorf-image.sh"

CANDIDATE_FINGERPRINT="$(incus image info "$CANDIDATE_ALIAS" | sed -n 's/^Fingerprint: //p')"
if [[ ! "$CANDIDATE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Could not resolve candidate image fingerprint." >&2
  exit 1
fi

mkdir -p "$EVIDENCE_DIR"

prove_harness() {
  local harness="$1"
  local profile_name="release-$harness"
  local goal_file="$PROOF_ROOT/$harness-goal.txt"
  local admission inspection
  if "$BINARY" profile show "$profile_name" >/dev/null 2>&1; then
    "$BINARY" profile update "$profile_name" \
      --sandbox-provider incus --image "$CANDIDATE_ALIAS" --network "$CANDIDATE_NETWORK" \
      --disk-size "$CANDIDATE_ROOT_DISK_SIZE" --harness "$harness"
  else
    "$BINARY" profile create "$profile_name" \
      --sandbox-provider incus --image "$CANDIDATE_ALIAS" --network "$CANDIDATE_NETWORK" \
      --disk-size "$CANDIDATE_ROOT_DISK_SIZE" --harness "$harness"
  fi
  "$BINARY" profile verify "$profile_name"
  "$BINARY" doctor --ai-connection "$AI_CONNECTION" --profile "$profile_name"
  printf '%s\n' \
    'Inspect the cloned repository without modifying it. Report the exact Git Revision, Debian release, and installed Codex, Pi, Git, Go, Python, Node, and uv versions. Keep the response concise.' \
    >"$goal_file"
  admission="$($BINARY admit \
    --key "image-proof:$harness:$BUILD_ID:$CANDIDATE_FINGERPRINT" \
    --goal-file "$goal_file" \
    --repo https://github.com/aphronio/dorf.git \
    --revision "$SOURCE_COMMIT" \
    --branch "dorf/image-proof-$harness-$BUILD_ID" \
    --base "${BASE_BRANCH:-main}" \
    --ai-connection "$AI_CONNECTION" \
    --profile "$profile_name" \
    --model gpt-5.6-sol \
    --reasoning low)"
  JOB_ID="$(jq -er .job_id <<<"$admission")"
  inspection="$(drive_job_until "$JOB_ID" '.observed_facts.agent_runs | any(.message_id != null and .turn_outcome != null)' "$harness turn")"
  jq -e --arg harness "$harness" '.observed_facts.agent_runs | map(select(.harness == $harness and (.thread_id | length > 0) and .turn_outcome == "completed" and (.turn_id | length > 0))) | length == 1' <<<"$inspection" >/dev/null
  jq -e --arg source "$SOURCE_COMMIT" '
    (.observed_facts.messages | map(select(.sequence == 1)) | .[0].id) as $message_id |
    (.observed_facts.agent_runs | map(select(.message_id == $message_id)) | .[0].id) as $agent_run_id |
    .job.revision == $source and
    (.observed_facts.revisions | length == 1) and
    (.observed_facts.revisions[0].oid == $source and .observed_facts.revisions[0].generation == 0) and
    (.observed_facts.evidence | any(
      .kind == "git-revision" and
      .agent_run_id == $agent_run_id and
      .revision == $source and
      (.started_at | length > 0) and
      (.finished_at | length > 0)
    )) and
    ([.observed_facts.agent_runs[] | select(.role != "implement")] | length == 0) and
    .proposal == null
  ' <<<"$inspection" >/dev/null

  "$BINARY" cleanup "$JOB_ID"
  inspection="$(drive_job_until "$JOB_ID" '.job.cleanup_state == "complete"' "$harness cleanup")"
  jq -e '.job.cleanup_state == "complete"' <<<"$inspection" >/dev/null
  jq -n \
    --arg harness "$harness" \
    --arg image "$CANDIDATE_ALIAS" \
    --arg fingerprint "$CANDIDATE_FINGERPRINT" \
    --arg source "$SOURCE_COMMIT" \
    --arg provider "$AI_CONNECTION" \
    --arg job "$JOB_ID" \
    '{schema_version:4,harness:$harness,image:{alias:$image,fingerprint:$fingerprint},source_commit:$source,provider_connection:$provider,job_id:$job,proof_scope:"one real no-change implementation AgentRun",observed:{implementation_agent_run:"completed",revision_history:"one initial Revision at generation 0",git_revision_evidence:"exact unchanged source Revision owned by the AgentRun",repository_commit_action:"absent; the AgentRun owns commits",workflow_result:"Message handled without a committed change; derived from Evidence",review:"not run or claimed",publication:"not run or claimed"},execution:"Go durable Core",cleanup_state:"complete"}' \
    >"$EVIDENCE_DIR/$harness-image-proof.json"
  JOB_ID=""
}

prove_harness codex
prove_harness pi

incus image export "$CANDIDATE_ALIAS" "$OUTPUT_DIR/$ARCHIVE_BASENAME" --vm
"$BINARY" release-manifest \
  --archive "$ARCHIVE_PATH" \
  --image-metadata "$METADATA_PATH" \
  --release-tag "$RELEASE_TAG" \
  --source-commit "$SOURCE_COMMIT" \
  --validated-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --output "$MANIFEST_PATH"

if [[ "$EVIDENCE_POLICY" == "remove" ]]; then
  rm -rf -- "$EVIDENCE_DIR"
else
  echo "Workstation evidence retained: $EVIDENCE_DIR"
fi
printf '%s\n' \
  "Incus image candidate ready: $RELEASE_TAG" \
  "Archive: $ARCHIVE_PATH" \
  "Manifest: $MANIFEST_PATH"
