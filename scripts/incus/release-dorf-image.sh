#!/usr/bin/env bash
set -euo pipefail

PUBLISH=false
if [[ "${1:-}" == "--publish" ]]; then
  PUBLISH=true
  shift
fi
if [[ $# -ne 0 ]]; then
  echo "usage: $0 [--publish]" >&2
  exit 2
fi
if [[ -z "${PROVIDER_CONNECTION:-}" ]]; then
  echo "Set PROVIDER_CONNECTION to one connected Provider Gateway name." >&2
  exit 2
fi
if [[ -z "${GITHUB_INSTALLATION_ID:-}" ]]; then
  echo "Set GITHUB_INSTALLATION_ID to the Dorf GitHub App installation used by the real proof." >&2
  exit 2
fi

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly GITHUB_REPOSITORY="${GITHUB_REPOSITORY:-aphronio/dorf}"
readonly OUTPUT_DIR="${OUTPUT_DIR:-$PROJECT_ROOT/dist/incus-image}"
readonly NETWORK="${NETWORK:-incusbr0}"
readonly ROOT_DISK_SIZE="${ROOT_DISK_SIZE:-40GiB}"
readonly SOURCE_COMMIT="${SOURCE_COMMIT:-$(git -C "$PROJECT_ROOT" rev-parse HEAD)}"
readonly BUILD_ID="$(date -u +%Y%m%d%H%M%S)"
readonly CANDIDATE_ALIAS="dorf-candidate-$BUILD_ID"
readonly BUILD_VM="dorf-build-$BUILD_ID"
readonly ARCHIVE_BASENAME="dorf-incus-vm-v5-x86_64"
readonly ARCHIVE_PATH="$OUTPUT_DIR/$ARCHIVE_BASENAME.tar.gz"
readonly MANIFEST_PATH="$OUTPUT_DIR/$ARCHIVE_BASENAME.json"
readonly METADATA_PATH="$OUTPUT_DIR/image.json"
readonly EVIDENCE_DIR="$OUTPUT_DIR/workstation-evidence"
readonly EVIDENCE_POLICY="${EVIDENCE_POLICY:-retain}"
readonly PROOF_ROOT="$(mktemp -d)"
readonly BINARY="$PROOF_ROOT/dorf"
JOB_ID=""

cleanup() {
  if [[ -n "$JOB_ID" ]]; then
    "$BINARY" cleanup "$JOB_ID" >/dev/null 2>&1 || true
    "$BINARY" worker --once >/dev/null 2>&1 || true
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

if [[ "$PUBLISH" == true ]]; then
  if ! command -v gh >/dev/null 2>&1; then
    echo "Required publication command is unavailable: gh" >&2
    exit 1
  fi
  if [[ "$(gh api "repos/$GITHUB_REPOSITORY" --jq .visibility)" != "public" ]]; then
    echo "Official Sandbox images require a public GitHub repository." >&2
    exit 1
  fi
  if [[ "$(gh variable get DORF_IMMUTABLE_RELEASES_ENABLED \
    --repo "$GITHUB_REPOSITORY" --json value --jq .value 2>/dev/null || true)" != "true" ]]; then
    echo "Enable GitHub release immutability, then set DORF_IMMUTABLE_RELEASES_ENABLED=true." >&2
    exit 1
  fi
  if ! gh api "repos/$GITHUB_REPOSITORY/commits/$SOURCE_COMMIT" >/dev/null; then
    echo "Source commit is not available from GitHub: $SOURCE_COMMIT" >&2
    exit 1
  fi
fi

mkdir -p "$OUTPUT_DIR"
rm -f "$METADATA_PATH" "$ARCHIVE_PATH" "$MANIFEST_PATH"
go -C "$PROJECT_ROOT" build -o "$BINARY" ./cmd/dorf

IMAGE_ALIAS="$CANDIDATE_ALIAS" \
BUILD_VM="$BUILD_VM" \
NETWORK="$NETWORK" \
ROOT_DISK_SIZE="$ROOT_DISK_SIZE" \
IMAGE_METADATA_PATH="$METADATA_PATH" \
  "$SCRIPT_DIR/build-dorf-image.sh"

CANDIDATE_FINGERPRINT="$(incus image info "$CANDIDATE_ALIAS" | sed -n 's/^Fingerprint: //p')"
if [[ ! "$CANDIDATE_FINGERPRINT" =~ ^[0-9a-f]{64}$ ]]; then
  echo "Could not resolve candidate image fingerprint." >&2
  exit 1
fi

export DORF_INCUS_IMAGE="$CANDIDATE_ALIAS"
export DORF_INCUS_NETWORK="$NETWORK"
export DORF_INCUS_DISK_SIZE="$ROOT_DISK_SIZE"
"$BINARY" doctor --provider "$PROVIDER_CONNECTION"
mkdir -p "$EVIDENCE_DIR"

prove_harness() {
  local harness="$1"
  local goal_file="$PROOF_ROOT/$harness-goal.txt"
  local admission inspection
  export DORF_HARNESS="$harness"
  printf '%s\n' \
    'Inspect the cloned repository without modifying it. Report the exact Git Revision, Debian release, and installed Codex, Pi, Git, Go, Python, Node, and uv versions. Keep the response concise.' \
    >"$goal_file"
  admission="$($BINARY admit \
    --key "image-proof:$harness:$BUILD_ID:$CANDIDATE_FINGERPRINT" \
    --goal-file "$goal_file" \
    --repo https://github.com/aphronio/dorf.git \
    --revision "$SOURCE_COMMIT" \
    --branch "dorf/image-proof-$harness-$BUILD_ID" \
    --github-repo aphronio/dorf \
    --github-installation "$GITHUB_INSTALLATION_ID" \
    --base "${BASE_BRANCH:-main}" \
    --provider "$PROVIDER_CONNECTION" \
    --model gpt-5.6-sol \
    --reasoning low)"
  JOB_ID="$(jq -er .job_id <<<"$admission")"
  "$BINARY" worker --once
  inspection="$($BINARY inspect --json "$JOB_ID")"
  jq -e '.observed_facts.actions | any(.kind == "repository-setup" and .state == "succeeded")' <<<"$inspection" >/dev/null
  jq -e --arg harness "$harness" '.observed_facts.messages | map(select(.sequence == 1 and .harness == $harness and (.thread_id | length > 0) and .turn_outcome == "completed" and (.turn_id | length > 0))) | length == 1' <<<"$inspection" >/dev/null
  jq -e --arg source "$SOURCE_COMMIT" '
    (.observed_facts.messages | map(select(.sequence == 1)) | .[0].agent_run_id) as $agent_run_id |
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
    (.observed_facts.checks | length == 0) and
    ([.observed_facts.agent_runs[] | select(.role != "implement")] | length == 0) and
    .proposal == null
  ' <<<"$inspection" >/dev/null

  "$BINARY" cleanup "$JOB_ID"
  "$BINARY" worker --once
  inspection="$($BINARY inspect --json "$JOB_ID")"
  jq -e '.job.cleanup_state == "complete"' <<<"$inspection" >/dev/null
  jq -n \
    --arg harness "$harness" \
    --arg image "$CANDIDATE_ALIAS" \
    --arg fingerprint "$CANDIDATE_FINGERPRINT" \
    --arg source "$SOURCE_COMMIT" \
    --arg provider "$PROVIDER_CONNECTION" \
    --arg job "$JOB_ID" \
    '{schema_version:4,harness:$harness,image:{alias:$image,fingerprint:$fingerprint},source_commit:$source,provider_connection:$provider,job_id:$job,proof_scope:"repository setup and one real no-change implementation AgentRun",observed:{repository_setup:"succeeded",implementation_agent_run:"completed",revision_history:"one initial Revision at generation 0",git_revision_evidence:"exact unchanged source Revision owned by the AgentRun",repository_commit_action:"absent; the AgentRun owns commits",workflow_result:"Message handled without a committed change; derived from Evidence",checks:"not run or claimed",review:"not run or claimed",publication:"not run or claimed"},execution:"Go durable Job spine",cleanup_state:"complete"}' \
    >"$EVIDENCE_DIR/$harness-image-proof.json"
  JOB_ID=""
}

prove_harness codex
prove_harness pi
unset DORF_HARNESS

incus image export "$CANDIDATE_ALIAS" "$OUTPUT_DIR/$ARCHIVE_BASENAME" --vm
PRODUCT_VERSION="$($BINARY version | awk '{print $2}')"
RELEASE_TAG="${RELEASE_TAG:-v$PRODUCT_VERSION}"
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
  "Candidate ready: $RELEASE_TAG" \
  "Archive: $ARCHIVE_PATH" \
  "Manifest: $MANIFEST_PATH"

if [[ "$PUBLISH" != true ]]; then
  exit
fi

"$PROJECT_ROOT/scripts/build-release.sh" "$OUTPUT_DIR"
if [[ "$RELEASE_TAG" != "v$PRODUCT_VERSION" ]]; then
  echo "Image release tag must match Go product version v$PRODUCT_VERSION: $RELEASE_TAG" >&2
  exit 1
fi
if gh release view "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; then
  echo "Release already exists: $RELEASE_TAG" >&2
  exit 1
fi

NOTES_PATH="$(mktemp)"
trap 'rm -f "$NOTES_PATH"; cleanup' EXIT
CODEX_VERSION="$(jq -r .harnesses.codex.version "$MANIFEST_PATH")"
PI_VERSION="$(jq -r .harnesses.pi.version "$MANIFEST_PATH")"
BASE_IMAGE_REFERENCE="$(jq -r .base_image.reference "$MANIFEST_PATH")"
printf '%s\n' \
  "Dorf $PRODUCT_VERSION" \
  "" \
  "Go x86_64 Linux application and credential-free Incus VM image." \
  "The image was promoted after real Dorf Codex and Pi turns against one fingerprint." \
  "" \
  "Codex: $CODEX_VERSION" \
  "Pi: $PI_VERSION" \
  "Base: $BASE_IMAGE_REFERENCE" \
  "Environment: Incus VM" \
  "Architecture: x86_64" \
  "Source commit: $SOURCE_COMMIT" >"$NOTES_PATH"

gh release create "$RELEASE_TAG" \
  --repo "$GITHUB_REPOSITORY" \
  --draft \
  --generate-notes \
  --target "$SOURCE_COMMIT" \
  --title "Dorf $RELEASE_TAG" \
  --notes-file "$NOTES_PATH" \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_linux_x86_64.tar.gz" \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_checksums.txt" \
  "$ARCHIVE_PATH" \
  "$MANIFEST_PATH"
gh release edit "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY" --draft=false --latest
gh release verify "$RELEASE_TAG" --repo "$GITHUB_REPOSITORY"
for asset in \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_linux_x86_64.tar.gz" \
  "$OUTPUT_DIR/dorf_${PRODUCT_VERSION}_checksums.txt" \
  "$ARCHIVE_PATH" \
  "$MANIFEST_PATH"; do
  gh release verify-asset "$RELEASE_TAG" "$asset" --repo "$GITHUB_REPOSITORY"
done
echo "Published verified official Sandbox image: $RELEASE_TAG"
