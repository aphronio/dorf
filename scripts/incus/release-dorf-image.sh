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
readonly PROOF_PROFILE="${PROOF_PROFILE:?Set PROOF_PROFILE to a verified Incus profile on the proof deployment}"
readonly HOST_COMMAND="${DORF_HOST_COMMAND:-}"
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

dorf_host() {
  if [[ -n "$HOST_COMMAND" ]]; then
    "$HOST_COMMAND" "$@"
  else
    "$BINARY" "$@"
  fi
}

wait_for_cleanup() {
  local deadline=$((SECONDS + 600))
  while ((SECONDS < deadline)); do
    if "$BINARY" job inspect --output json "$JOB_ID" | jq -e '.cleanup.state == "complete"' >/dev/null; then
      return
    fi
    sleep 2
  done
  echo "Timed out waiting for cleanup on Job $JOB_ID." >&2
  return 1
}

cleanup() {
  if [[ -n "$JOB_ID" ]]; then
    "$BINARY" job cleanup "$JOB_ID" >/dev/null 2>&1 || true
    wait_for_cleanup >/dev/null 2>&1 || true
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
base_profile="$(dorf_host profile show "$PROOF_PROFILE")"
jq -e '.provider == "incus" and .verified == true' <<<"$base_profile" >/dev/null
proof_project="$(jq -er .incus_project <<<"$base_profile")"
incus image copy "local:$CANDIDATE_FINGERPRINT" local: --target-project "$proof_project"

prove_harness() {
  local harness="$1"
  local profile_name="release-$harness-$BUILD_ID"
  local goal_file="$PROOF_ROOT/$harness-goal.txt"
  local admission message_id message inspection deadline
  dorf_host profile create "$profile_name" \
    --sandbox-provider incus --image "$CANDIDATE_FINGERPRINT" --harness "$harness" \
    --project "$proof_project" \
    --storage-pool "$(jq -er .incus_storage_pool <<<"$base_profile")" \
    --network "$(jq -er .incus_network <<<"$base_profile")" \
    --disk-size "$(jq -er .incus_disk_size <<<"$base_profile")" \
    --gateway-url "$(jq -er .incus_gateway_url <<<"$base_profile")"
  dorf_host profile verify "$profile_name"
  printf '%s\n' \
    'Inspect the cloned repository without modifying it. Report the exact Git Revision, Debian release, and installed Codex, Pi, Git, Go, Python, Node, and uv versions.' \
    'Then use the installed browser-use CLI with its existing isolated browser. Read /root/.codex/skills/browser-use/SKILL.md. Navigate to https://example.com and click Learn more using browser-use. Report the resulting URL and title. Do not install or configure any browser tools. Do not modify repository files or make commits.' \
    >"$goal_file"
  admission="$("$BINARY" workflow run coding \
    --key "image-proof:$harness:$BUILD_ID:$CANDIDATE_FINGERPRINT" \
    --input-file "$goal_file" \
    --repo https://github.com/aphronio/dorf.git \
    --revision "$SOURCE_COMMIT" \
    --branch "dorf/image-proof-$harness-$BUILD_ID" \
    --base "${BASE_BRANCH:-main}" \
    --ai-connection "$AI_CONNECTION" \
    --profile "$profile_name" \
    --model "${PROOF_MODEL:-gpt-5.6-sol}" --reasoning low --output json)"
  JOB_ID="$(jq -er .job.id <<<"$admission")"
  message_id="$(jq -er .message.id <<<"$admission")"
  deadline=$((SECONDS + 3600))
  while ((SECONDS < deadline)); do
    message="$("$BINARY" job message inspect --output json "$JOB_ID" "$message_id")"
    if jq -e '.result != null or .attention != null' <<<"$message" >/dev/null; then
      break
    fi
    sleep 2
  done
  printf '%s\n' "$message" >"$EVIDENCE_DIR/$harness-message.json"
  jq -e '.result.outcome == "completed" and (.result.output | contains("https://www.iana.org/help/example-domains")) and (.result.output | contains("Example Domains"))' <<<"$message" >/dev/null
  inspection="$("$BINARY" job inspect --output json "$JOB_ID")"
  jq -e --arg source "$SOURCE_COMMIT" '.revision == $source and .proposal == null' <<<"$inspection" >/dev/null
  "$BINARY" job evidence --output json "$JOB_ID" >"$EVIDENCE_DIR/$harness-evidence.json"
  jq -e --arg source "$SOURCE_COMMIT" '.evidence | any(.kind == "git-revision" and .revision == $source)' "$EVIDENCE_DIR/$harness-evidence.json" >/dev/null
  "$BINARY" job cleanup "$JOB_ID"
  wait_for_cleanup
  "$BINARY" job inspect --output json "$JOB_ID" >"$EVIDENCE_DIR/$harness-image-proof.json"
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
