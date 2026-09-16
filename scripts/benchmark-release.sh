#!/usr/bin/env bash
set -euo pipefail

readonly PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
RUNS=3
REF=HEAD
OUTPUT=""
OVERLAYS=()

usage() {
  cat <<'EOF'
Usage: bash scripts/benchmark-release.sh [--runs N] [--ref REVISION]
       [--output NEW_PATH] [--overlay TRACKED_PATH ...]

Build and verify a disposable clean clone without publishing. Run 1 has empty
Go and Buildx caches; later runs reuse exported caches with fresh builders.
Defaults: HEAD, 3 runs, a new directory under .dorf/release-benchmarks/.
Repeat --overlay to copy explicit tracked files into a clone-only local commit.
Output must be a new directory under this repository's .dorf/.
Requires Linux Bash, git, mise, Docker/Buildx, jq and standard command-line tools;
the existing installer proof also requires python3 and curl.
EOF
}

fail() { printf 'release benchmark: %s\n' "$*" >&2; exit 1; }
while (( $# )); do
  case "$1" in
    --runs|--ref|--output|--overlay)
      (( $# >= 2 )) && [[ -n "$2" ]] || fail "$1 requires a value"
      case "$1" in
        --runs) RUNS="$2" ;;
        --ref) REF="$2" ;;
        --output) OUTPUT="$2" ;;
        --overlay) OVERLAYS+=("$2") ;;
      esac
      shift 2
      ;;
    --help|-h) usage; exit 0 ;;
    *) usage >&2; fail "unknown argument: $1" ;;
  esac
done
[[ "$RUNS" =~ ^[1-9][0-9]*$ && ${#RUNS} -le 4 ]] || fail '--runs must be an integer from 1 to 9999'
[[ -r /proc/uptime ]] || fail 'Linux /proc/uptime is required for monotonic elapsed time'
for command in git docker mise jq realpath sha256sum du python3 curl; do
  command -v "$command" >/dev/null || fail "required command is unavailable: $command"
done
readonly MISE="$(command -v "${DORF_MISE:-mise}")"
readonly BASE_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse --verify --end-of-options "$REF^{commit}")"

for path in "${OVERLAYS[@]}"; do
  case "$path" in
    /*|../*|*/../*|*/..|./*|*/./*|*/.|*//*|*$'\n'*|*$'\t'*) fail "overlay must be a plain repository-relative file path: $path" ;;
  esac
  git -C "$PROJECT_ROOT" ls-files --error-unmatch -- ":(literal)$path" >/dev/null ||
    fail "overlay is not tracked: $path"
  [[ -f "$PROJECT_ROOT/$path" && ! -L "$PROJECT_ROOT/$path" ]] ||
    fail "overlay must be an existing regular file: $path"
  [[ "$(realpath -- "$PROJECT_ROOT/$path")" == "$PROJECT_ROOT/"* ]] ||
    fail "overlay escapes the repository: $path"
done

OUTPUT="${OUTPUT:-$PROJECT_ROOT/.dorf/release-benchmarks/$(date -u +%Y%m%dT%H%M%SZ)-$$}"
OUTPUT="$(realpath -m -- "$OUTPUT")"
readonly RECEIPT_ROOT="$(realpath -m -- "$PROJECT_ROOT/.dorf")"
[[ "$OUTPUT" == "$RECEIPT_ROOT/"* ]] || fail '--output must be under this repository’s .dorf/'
case "$OUTPUT" in
  *,*|*$'\n'*|*$'\t'*) fail '--output cannot contain commas, tabs or newlines' ;;
esac
[[ ! -e "$OUTPUT" && ! -L "$OUTPUT" ]] || fail "output already exists: $OUTPUT"
mkdir -p -- "$(dirname -- "$OUTPUT")"
mkdir -- "$OUTPUT"
readonly OUTPUT
readonly SOURCE="$OUTPUT/source"
mkdir -- "$OUTPUT/logs" "$OUTPUT/cache" "$OUTPUT/runs"
printf 'run\tstate\tstep\telapsed_seconds\tstatus\tlog\n' >"$OUTPUT/steps.tsv"
printf 'run\tstate\tpoint\tcache\tbytes\n' >"$OUTPUT/cache-sizes.tsv"
BUILDER=""
RUN=0
STATE=setup
STEP=setup
LOG_DIR="$OUTPUT/logs"

# /proc/uptime is monotonic and independent of wall-clock adjustments.
clock_centiseconds() {
  local uptime ignored
  read -r uptime ignored </proc/uptime
  printf '%s\n' "${uptime/./}"
}

step() {
  STEP="$1"
  shift
  local start end status=0
  local log="$LOG_DIR/$STEP.log"
  printf '[%s/%s] %s (log: %s)\n' "$RUN" "$STATE" "$STEP" "$log"
  printf '%q ' "$@" >"$log"
  printf '\n' >>"$log"
  start="$(clock_centiseconds)"
  set +e
  (set -e; "$@") >>"$log" 2>&1
  status=$?
  set -e
  end="$(clock_centiseconds)"
  printf '%s\t%s\t%s\t%d.%02d\t%s\t%s\n' \
    "$RUN" "$STATE" "$STEP" "$(((end - start) / 100))" "$(((end - start) % 100))" \
    "$status" "${log#"$OUTPUT/"}" >>"$OUTPUT/steps.tsv"
  if (( status != 0 )); then
    printf 'Failed: %s (status %s); retained log: %s\n' "$STEP" "$status" "$log" >&2
  fi
  return "$status"
}

cleanup() {
  local status=$? cleanup_status=0 failed_step="$STEP"
  trap - EXIT
  if [[ -n "$BUILDER" ]]; then
    step builder-cleanup docker buildx rm --force "$BUILDER" || cleanup_status=$?
    if (( status == 0 && cleanup_status != 0 )); then
      status="$cleanup_status"
      failed_step=builder-cleanup
    fi
  fi
  printf '{"exit_status":%d,"last_step":"%s","run":%d}\n' \
    "$status" "$failed_step" "$RUN" >"$OUTPUT/result.json"
  printf 'Release benchmark receipt: %s (status %s)\n' "$OUTPUT" "$status"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

prepare_source() {
  git -c core.hooksPath=/dev/null clone --no-hardlinks --no-checkout -- "$PROJECT_ROOT" "$SOURCE" || return
  git -C "$SOURCE" config core.hooksPath /dev/null || return
  git -C "$SOURCE" checkout --detach "$BASE_COMMIT" || return
  git -C "$SOURCE" config user.name 'Release Benchmark' || return
  git -C "$SOURCE" config user.email 'release-benchmark@example.invalid' || return
  git -C "$SOURCE" config commit.gpgsign false || return
  # Make accidental pushes from this retained disposable clone fail locally.
  git -C "$SOURCE" remote remove origin || return
  local path
  for path in "${OVERLAYS[@]}"; do
    git -C "$SOURCE" ls-files --error-unmatch -- ":(literal)$path" >/dev/null || return
    [[ -f "$SOURCE/$path" && ! -L "$SOURCE/$path" ]] || return 1
    [[ "$(realpath -- "$SOURCE/$path")" == "$SOURCE/"* ]] || return 1
    cp --preserve=mode -- "$PROJECT_ROOT/$path" "$SOURCE/$path" || return
    git -C "$SOURCE" add -- ":(literal)$path" || return
  done
  if ! git -C "$SOURCE" diff --cached --quiet; then
    GIT_AUTHOR_NAME='Release Benchmark' GIT_AUTHOR_EMAIL='release-benchmark@example.invalid' \
      GIT_COMMITTER_NAME='Release Benchmark' GIT_COMMITTER_EMAIL='release-benchmark@example.invalid' \
      git -C "$SOURCE" commit --signoff -m 'Apply explicit local release benchmark overlays' || return
  fi
  [[ -z "$(git -C "$SOURCE" status --porcelain --untracked-files=all)" ]]
}

cache_sizes() {
  local point="$1" name bytes ignored
  for name in go-build go-mod buildx; do
    bytes=0
    if [[ -d "$OUTPUT/cache/$name" ]]; then
      read -r bytes ignored < <(du -sb -- "$OUTPUT/cache/$name")
    fi
    printf '%s\t%s\t%s\t%s\t%s\n' "$RUN" "$STATE" "$point" "$name" "$bytes" >>"$OUTPUT/cache-sizes.tsv"
  done
}

step source prepare_source
printf '%s\n' "${OVERLAYS[@]}" >"$OUTPUT/overlays.txt"
git -C "$SOURCE" diff --binary "$BASE_COMMIT" HEAD >"$OUTPUT/overlay.patch"
git -C "$SOURCE" show -s --format=fuller HEAD >"$OUTPUT/source-commit.txt"
sha256sum -- "$PROJECT_ROOT/scripts/benchmark-release.sh" >"$OUTPUT/driver.sha256"
jq -n --arg ref "$REF" --arg base "$BASE_COMMIT" \
  --arg source "$(git -C "$SOURCE" rev-parse HEAD)" --argjson runs "$RUNS" \
  --arg receipt "$OUTPUT" --arg started "$(date -u +%FT%TZ)" \
  '{ref: $ref, base_commit: $base, source_commit: $source, runs: $runs,
    receipt: $receipt, started_at: $started, timing: "Linux /proc/uptime (0.01 seconds)",
    limitations: ["Host OS, CPU, RAM, disk and contention differ from hosted runners",
      "Mise tool downloads and Docker daemon image downloads may already be cached",
      "Warm Go caches and exported Buildx cache are local, not GitHub cache transfers",
      "Local image export/load and runtime proofs replace registry publishing",
      "Loaded semantic-version image remains in the Docker daemon; no shared resources are pruned",
      "Authority tests use their own fixtures; the preceding release build and installer artifact are real"]}' \
  >"$OUTPUT/metadata.json"
{
  uname -a
  printf '\nCPU count: '; nproc
  printf '\nMemory:\n'; cat /proc/meminfo
  printf '\nCPU:\n'; cat /proc/cpuinfo
  printf '\nFilesystem:\n'; df -h -- "$OUTPUT"
  printf '\nBash: %s\n' "$BASH_VERSION"
  git --version
  "$MISE" --version
  jq --version
} >"$OUTPUT/host.txt"
step docker-version docker version
step buildx-version docker buildx version
step docker-info docker info
export MISE_ENABLE_TOOLS=go
export MISE_LOCKED=1
step mise-trust "$MISE" -C "$SOURCE" trust --yes
step mise-install "$MISE" -C "$SOURCE" install --locked go

export GOCACHE="$OUTPUT/cache/go-build"
export GOMODCACHE="$OUTPUT/cache/go-mod"
export DORF_BUILDX_CACHE="$OUTPUT/cache/buildx"
export DORF_MISE="$MISE"
export BUILDKIT_PROGRESS=plain
mkdir -- "$GOCACHE" "$GOMODCACHE"
step go-environment "$MISE" -C "$SOURCE" exec -- go env -json

cd -- "$SOURCE"

for (( RUN=1; RUN<=RUNS; RUN++ )); do
  STATE=warm
  (( RUN != 1 )) || STATE=cold
  RUN_DIR="$OUTPUT/runs/$RUN-$STATE"
  mkdir -- "$RUN_DIR" "$RUN_DIR/logs" "$RUN_DIR/artifacts"
  LOG_DIR="$RUN_DIR/logs"
  cache_sizes before
  BUILDER="dorf-bench-$(date +%s)-$$-$RUN-$RANDOM"
  printf '%s\n' "$BUILDER" >"$RUN_DIR/builder.txt"
  export BUILDX_BUILDER="$BUILDER"
  step builder-create docker buildx create --name "$BUILDER" --driver docker-container
  step builder-bootstrap docker buildx inspect --bootstrap "$BUILDER"
  step build bash "$SOURCE/scripts/build-release.sh" "$RUN_DIR/artifacts"
  step install-test bash "$SOURCE/scripts/install-test.sh" "$RUN_DIR/artifacts/install.sh" "$RUN_DIR/artifacts"
  step release-test bash "$SOURCE/scripts/release-test.sh"
  cache_sizes after
  [[ ! -f "$DORF_BUILDX_CACHE/index.json" ]] || cp -- "$DORF_BUILDX_CACHE/index.json" "$RUN_DIR/buildx-index.json"
  step builder-remove docker buildx rm --force "$BUILDER"
  BUILDER=""
done
RUN="$RUNS"

