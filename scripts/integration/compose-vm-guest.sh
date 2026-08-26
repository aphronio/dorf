#!/usr/bin/env bash
set -euo pipefail

readonly KVM_DEVICE="${DORF_PROOF_KVM_DEVICE:-/dev/kvm}"
readonly CPUINFO="${DORF_PROOF_CPUINFO:-/proc/cpuinfo}"
readonly PROFILE_NAME="compose-vm-proof"
readonly CONNECTION_NAME="openai-api"
readonly MAX_EVIDENCE_BYTES=262144

readonly COMPOSE_DIR="$HOME/.local/share/dorf-compose"

DORF_BIN=
COMPOSE_MANIFEST=
COMPOSE_INCUS_OVERLAY=
SECRET_FILE=
EVIDENCE_DIR=
JOB_ID=
SANDBOX_ID=
EPHEMERAL_SECRET=
EPHEMERAL_KEY_PATH=
PROOF_COMPLETE=0

usage() {
	cat <<'EOF'
Usage:
  compose-vm-guest.sh cache-prep --user USER \
    --docker-helper PATH --docker-sha256 SHA256 \
    --incus-helper PATH --incus-sha256 SHA256 --guest-sha256 SHA256
  compose-vm-guest.sh prove --app-archive PATH --checksums PATH \
    --image-ref REF --sandbox-archive PATH --sandbox-manifest PATH \
    --openai-key-file PATH --work-root PATH --evidence-dir PATH

cache-prep is an explicit administrator phase. prove runs as the current
deployment operator when Docker, Compose, Incus, and KVM are ready for that identity.
EOF
}

die() {
	printf 'compose-vm-guest: %s\n' "$1" >&2
	exit 1
}

require_sha256() {
	local path=$1 expected=$2 label=$3 observed
	[[ "$expected" =~ ^[0-9a-f]{64}$ ]] || die "$label digest is invalid"
	[[ -f "$path" && ! -L "$path" ]] || die "$label is not one regular file"
	observed=$(sha256sum "$path" | awk '{print $1}')
	[[ "$observed" == "$expected" ]] || die "$label differs from the cache identity"
}

require_nested_kvm() {
	[[ -c "$KVM_DEVICE" ]] || die "nested virtualization is unavailable: $KVM_DEVICE is not a KVM character device"
	grep -Eq '(^|[[:space:]])(vmx|svm)($|[[:space:]])' "$CPUINFO" ||
		die "nested virtualization is unavailable: the guest CPU exposes neither vmx nor svm"
}

as_user() {
	local user=$1
	shift
	runuser -u "$user" -- env -i \
		HOME="$(getent passwd "$user" | awk -F: '{print $6}')" \
		USER="$user" PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin "$@"
}

cache_prep() {
	[[ "$(id -u)" -eq 0 ]] || die "cache-prep must be run by the test-harness administrator"
	local user= docker_helper= docker_sha= incus_helper= incus_sha= guest_sha=
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--user) [[ $# -ge 2 ]] || die "--user needs a value"; user=$2; shift 2 ;;
		--docker-helper) [[ $# -ge 2 ]] || die "--docker-helper needs a value"; docker_helper=$2; shift 2 ;;
		--docker-sha256) [[ $# -ge 2 ]] || die "--docker-sha256 needs a value"; docker_sha=$2; shift 2 ;;
		--incus-helper) [[ $# -ge 2 ]] || die "--incus-helper needs a value"; incus_helper=$2; shift 2 ;;
		--incus-sha256) [[ $# -ge 2 ]] || die "--incus-sha256 needs a value"; incus_sha=$2; shift 2 ;;
		--guest-sha256) [[ $# -ge 2 ]] || die "--guest-sha256 needs a value"; guest_sha=$2; shift 2 ;;
		*) die "cache-prep received unknown argument '$1'" ;;
		esac
	done
	[[ "$user" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "cache-prep requires one safe operator account"
	[[ "$(id -u "$user")" -ne 0 ]] || die "cache-prep helpers require a separate non-root operator account"
	require_nested_kvm
	require_sha256 "$docker_helper" "$docker_sha" docker.sh
	require_sha256 "$incus_helper" "$incus_sha" incus.sh
	require_sha256 "$(realpath -e -- "$0")" "$guest_sha" compose-vm-guest.sh

	"$docker_helper" --user "$user" \
		--acknowledge-docker-root-authority \
		--acknowledge-firewall-impact
	"$incus_helper" --user "$user" \
		--acknowledge-incus-root-authority \
		--acknowledge-kvm-device-access \
		--initialize-pristine

	id -nG "$user" | tr ' ' '\n' | grep -Fx docker >/dev/null || die "operator account $user did not receive Docker authority"
	id -nG "$user" | tr ' ' '\n' | grep -Fx incus-admin >/dev/null || die "operator account $user did not receive Incus authority"
	id -nG "$user" | tr ' ' '\n' | grep -Fx kvm >/dev/null || die "operator account $user did not receive KVM access"
	as_user "$user" test -r "$KVM_DEVICE"
	as_user "$user" test -w "$KVM_DEVICE"
	as_user "$user" docker info >/dev/null
	as_user "$user" docker compose version >/dev/null

	local docker_containers docker_images docker_volumes incus_instances incus_images
	docker_containers=$(as_user "$user" docker ps -aq) || die "operator account $user cannot inventory Docker containers"
	docker_images=$(as_user "$user" docker image ls -q) || die "operator account $user cannot inventory Docker images"
	docker_volumes=$(as_user "$user" docker volume ls -q) || die "operator account $user cannot inventory Docker volumes"
	incus_instances=$(as_user "$user" incus --force-local --project dorf list --format csv -c n) || die "operator account $user cannot inventory the restricted Incus project"
	incus_images=$(as_user "$user" incus --force-local --project dorf image list --format csv -c f) || die "operator account $user cannot inventory restricted Incus images"
	[[ -z "$docker_containers" ]] || die "cache contains Docker containers"
	[[ -z "$docker_images" ]] || die "cache contains Docker images"
	[[ -z "$docker_volumes" ]] || die "cache contains Docker volumes"
	[[ -z "$incus_instances" ]] || die "cache contains an Incus instance"
	[[ -z "$incus_images" ]] || die "cache contains a Dorf project image"

	local user_home
	user_home=$(getent passwd "$user" | awk -F: '{print $6}')
	for path in \
		"$user_home/.config/dorf" \
		"$user_home/.local/share/dorf" \
		"$user_home/.local/share/dorf-compose" \
		"$user_home/.local/state/dorf"; do
		[[ ! -e "$path" && ! -L "$path" ]] || die "cache contains Dorf state at $path"
	done
	if as_user "$user" sh -c 'command -v dorf >/dev/null 2>&1'; then
		die "cache contains a Dorf binary"
	fi
	printf 'CACHE PREP -> Docker + Compose -> empty restricted Incus project -> frozen\n'
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required proof command is unavailable: $1"
}

canonical_input_file() {
	local path=$1 label=$2
	[[ -f "$path" && ! -L "$path" ]] || die "$label must be one regular non-symlink file"
	realpath -e -- "$path"
}

sanitize_evidence_file() {
	local file=$1 size
	[[ -f "$file" && ! -L "$file" ]] || return 0
	size=$(stat -c '%s' "$file")
	if ((size > MAX_EVIDENCE_BYTES)); then
		printf '[discarded: evidence exceeded 256 KiB bound]\n' >"$file"
	elif [[ -n "$EPHEMERAL_SECRET" ]] && grep -Fq -- "$EPHEMERAL_SECRET" "$file"; then
		printf '[redacted: ephemeral secret detected]\n' >"$file"
	fi
}

capture() {
	local destination=$1
	shift
	local temporary status
	temporary=$(mktemp "$EVIDENCE_DIR/.capture.XXXXXXXX")
	set +e
	"$@" >"$temporary" 2>&1
	status=$?
	set -e
	head -c "$MAX_EVIDENCE_BYTES" "$temporary" >"$destination"
	rm -f -- "$temporary"
	chmod 0600 "$destination"
	sanitize_evidence_file "$destination"
	return "$status"
}

capture_failure_evidence() {
	[[ -n "$EVIDENCE_DIR" && -d "$EVIDENCE_DIR" ]] || return 0
	if [[ -n "$COMPOSE_MANIFEST" && -f "$COMPOSE_MANIFEST" && -f "$COMPOSE_INCUS_OVERLAY" && -f "$COMPOSE_DIR/.env" ]]; then
		capture "$EVIDENCE_DIR/failure-compose-status.json" compose ps --all --format json || true
		capture "$EVIDENCE_DIR/failure-api.log" compose logs --no-color --tail 200 control-api || true
		capture "$EVIDENCE_DIR/failure-worker.log" compose logs --no-color --tail 200 worker || true
	fi
	if [[ -n "$DORF_BIN" && -x "$DORF_BIN" ]]; then
		if [[ -n "$JOB_ID" ]]; then
			capture "$EVIDENCE_DIR/failure-inspect.json" "$DORF_BIN" inspect --json "$JOB_ID" || true
		fi
	fi
	capture "$EVIDENCE_DIR/failure-incus.json" incus --force-local --project dorf list --format json || true
}

cleanup_proof() {
	local status=$?
	trap - EXIT
	if [[ "$status" -ne 0 && "$PROOF_COMPLETE" -ne 1 ]]; then
		capture_failure_evidence || true
	fi
	if [[ -n "$EPHEMERAL_KEY_PATH" ]]; then
		rm -f -- "$EPHEMERAL_KEY_PATH"
	fi
	EPHEMERAL_SECRET=
	exit "$status"
}

arm_ephemeral_key_cleanup() {
	local args=("$@")
	local index candidate count=0
	for ((index = 0; index < ${#args[@]}; index++)); do
		[[ "${args[$index]}" == --openai-key-file ]] || continue
		((count += 1))
	done
	[[ "$count" -le 1 ]] || die "--openai-key-file may be provided exactly once"
	for ((index = 0; index < ${#args[@]}; index++)); do
		[[ "${args[$index]}" == --openai-key-file ]] || continue
		((index + 1 < ${#args[@]})) || return 0
		candidate=${args[$((index + 1))]}
		if [[ -f "$candidate" && ! -L "$candidate" ]] &&
			[[ "$(stat -c '%a' "$candidate")" == 600 && "$(stat -c '%u' "$candidate")" == "$(id -u)" ]]; then
			EPHEMERAL_KEY_PATH=$(realpath -e -- "$candidate")
			trap cleanup_proof EXIT
		fi
		return 0
	done
}

parse_proof_options() {
	APP_ARCHIVE=
	CHECKSUMS=
	SANDBOX_ARCHIVE=
	SANDBOX_MANIFEST=
	IMAGE_REF=
	SECRET_FILE=
	WORK_ROOT=
	EVIDENCE_DIR=
	local openai_key_count=0
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--app-archive) [[ $# -ge 2 ]] || die "--app-archive needs a value"; APP_ARCHIVE=$2; shift 2 ;;
		--checksums) [[ $# -ge 2 ]] || die "--checksums needs a value"; CHECKSUMS=$2; shift 2 ;;
		--image-ref) [[ $# -ge 2 ]] || die "--image-ref needs a value"; IMAGE_REF=$2; shift 2 ;;
		--sandbox-archive) [[ $# -ge 2 ]] || die "--sandbox-archive needs a value"; SANDBOX_ARCHIVE=$2; shift 2 ;;
		--sandbox-manifest) [[ $# -ge 2 ]] || die "--sandbox-manifest needs a value"; SANDBOX_MANIFEST=$2; shift 2 ;;
		--openai-key-file)
			[[ $# -ge 2 ]] || die "--openai-key-file needs a value"
			((openai_key_count += 1))
			[[ "$openai_key_count" -eq 1 ]] || die "--openai-key-file may be provided exactly once"
			SECRET_FILE=$2
			shift 2
			;;
		--work-root) [[ $# -ge 2 ]] || die "--work-root needs a value"; WORK_ROOT=$2; shift 2 ;;
		--evidence-dir) [[ $# -ge 2 ]] || die "--evidence-dir needs a value"; EVIDENCE_DIR=$2; shift 2 ;;
		*) die "prove received unknown argument '$1'" ;;
		esac
	done
	[[ -n "$IMAGE_REF" ]] || die "prove requires --image-ref"
	[[ "$IMAGE_REF" =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]] || die "image reference is invalid"
	local evidence_arg=$EVIDENCE_DIR
	EVIDENCE_DIR=
	SECRET_FILE=$(canonical_input_file "$SECRET_FILE" "--openai-key-file")
	[[ "$(stat -c '%a' "$SECRET_FILE")" == 600 && "$(stat -c '%u' "$SECRET_FILE")" == "$(id -u)" ]] ||
		die "ephemeral key file must be operator-owned mode 0600"
	trap cleanup_proof EXIT
	EPHEMERAL_KEY_PATH=$SECRET_FILE
	APP_ARCHIVE=$(canonical_input_file "$APP_ARCHIVE" "--app-archive")
	CHECKSUMS=$(canonical_input_file "$CHECKSUMS" "--checksums")
	SANDBOX_ARCHIVE=$(canonical_input_file "$SANDBOX_ARCHIVE" "--sandbox-archive")
	SANDBOX_MANIFEST=$(canonical_input_file "$SANDBOX_MANIFEST" "--sandbox-manifest")
	EVIDENCE_DIR=$evidence_arg
	[[ "$WORK_ROOT" = /* && "$EVIDENCE_DIR" = /* ]] || die "proof work and evidence paths must be absolute"
	[[ ! -e "$WORK_ROOT" && ! -L "$WORK_ROOT" ]] || die "proof work root must be fresh"
	[[ -d "$EVIDENCE_DIR" && ! -L "$EVIDENCE_DIR" ]] || die "proof evidence directory must be one real directory"
	[[ "$(stat -c '%u' "$EVIDENCE_DIR")" == "$(id -u)" ]] || die "proof evidence directory must be operator-owned"
	install -d -m 0700 "$WORK_ROOT"
	EPHEMERAL_SECRET=$(<"$SECRET_FILE")
	[[ -n "$EPHEMERAL_SECRET" && "$EPHEMERAL_SECRET" != *$'\n'* ]] || die "ephemeral AI key must be one non-empty line"
}

assert_fresh_cache() {
	local containers images volumes
	containers=$(docker ps -aq) || die "proof operator cannot inventory Docker containers"
	images=$(docker image ls -q) || die "proof operator cannot inventory Docker images"
	volumes=$(docker volume ls -q) || die "proof operator cannot inventory Docker volumes"
	[[ -z "$containers" ]] || die "fresh proof VM already contains Docker containers"
	[[ -z "$images" ]] || die "fresh release-proof VM already contains Docker images"
	[[ -z "$volumes" ]] || die "fresh proof VM already contains Docker volumes"
	incus --force-local --project dorf query /1.0 >/dev/null
	incus --force-local --project dorf list --format json | jq -e 'length == 0' >/dev/null || die "fresh proof VM contains an Incus instance"
	incus --force-local --project dorf image list --format json | jq -e 'length == 0' >/dev/null || die "fresh proof VM contains a Dorf project image"
	for path in "$HOME/.config/dorf" "$HOME/.local/share/dorf" "$HOME/.local/share/dorf-compose" "$HOME/.local/state/dorf"; do
		[[ ! -e "$path" && ! -L "$path" ]] || die "fresh proof VM contains Dorf state at $path"
	done
	if command -v dorf >/dev/null 2>&1; then
		die "fresh proof VM contains an installed Dorf binary"
	fi
}

prepare_release() {
	local app_base checksums_base version expected_checksum observed_checksum official_image_ref manifest
	app_base=$(basename -- "$APP_ARCHIVE")
	[[ "$app_base" =~ ^dorf_([0-9]+\.[0-9]+\.[0-9]+)_linux_x86_64\.tar\.gz$ ]] || die "application archive name is invalid"
	version=${BASH_REMATCH[1]}
	checksums_base=$(basename -- "$CHECKSUMS")
	[[ "$checksums_base" == "dorf_${version}_checksums.txt" ]] || die "checksum release differs from application release"
	official_image_ref="ghcr.io/aphronio/dorf:$version"
	[[ "$IMAGE_REF" == "$official_image_ref" ]] || die "--image-ref must equal $official_image_ref"
	[[ "$(basename -- "$SANDBOX_ARCHIVE")" == dorf-incus-vm-v5-x86_64.tar.gz ]] || die "Sandbox archive name is invalid"
	[[ "$(basename -- "$SANDBOX_MANIFEST")" == dorf-incus-vm-v5-x86_64.json ]] || die "Sandbox manifest name is invalid"
	local staging="$WORK_ROOT/release-inputs"
	install -d -m 0700 "$staging"
	cp -- "$APP_ARCHIVE" "$CHECKSUMS" "$staging/"
	expected_checksum=$(awk -v file="$app_base" '$2 == file && $1 ~ /^[0-9a-f]{64}$/ {print $1}' "$staging/$checksums_base")
	[[ "$expected_checksum" =~ ^[0-9a-f]{64}$ ]] || die "release checksums must name the application archive exactly once"
	observed_checksum=$(sha256sum "$staging/$app_base" | awk '{print $1}')
	[[ "$observed_checksum" == "$expected_checksum" ]] || die "application archive checksum differs"
	install -d -m 0700 "$WORK_ROOT/release"
	tar -xzf "$staging/$app_base" -C "$WORK_ROOT/release"
	DORF_BIN="$WORK_ROOT/release/dorf"
	[[ -f "$DORF_BIN" && ! -L "$DORF_BIN" && -x "$DORF_BIN" ]] || die "application archive contains no executable Dorf binary"
	COMPOSE_MANIFEST="$WORK_ROOT/release/dorf-compose.yaml"
	COMPOSE_INCUS_OVERLAY="$WORK_ROOT/release/dorf-compose-incus.yaml"
	for manifest in "$COMPOSE_MANIFEST" "$COMPOSE_INCUS_OVERLAY"; do
		[[ -f "$manifest" && ! -L "$manifest" ]] || die "application archive is missing $(basename -- "$manifest")"
	done
	[[ "$("$DORF_BIN" version)" == "dorf $version" ]] || die "extracted binary version differs from the release asset"
}

compose() {
	(
		cd "$COMPOSE_DIR"
		docker compose "$@"
	)
}

assert_compose_project() {
	[[ -f "$COMPOSE_DIR/.env" && ! -L "$COMPOSE_DIR/.env" ]] ||
		die "setup did not render $COMPOSE_DIR/.env"
}

run_setup() {
	local -a setup_args=(
		--yes
		--sandbox-provider incus
		--profile "$PROFILE_NAME"
		--connection-auth openai
		--openai-api-key-file "$SECRET_FILE"
		--incus-manifest "$SANDBOX_MANIFEST"
		--incus-archive "$SANDBOX_ARCHIVE"
	)
	if ! capture "$EVIDENCE_DIR/setup.log" "$DORF_BIN" setup "${setup_args[@]}"; then
		die "setup did not apply and verify the Dorf deployment"
	fi
	grep -Fq -- "Dorf ready: Control plane and durable Job worker ready" "$EVIDENCE_DIR/setup.log" ||
		die "setup did not print its final ready receipt"
	if grep -Eq 'Dorf deployment configuration.*deployment guide|follow the deployment guide' "$EVIDENCE_DIR/setup.log"; then
		die "setup returned a stale deployment handoff"
	fi
	assert_compose_project
	capture "$EVIDENCE_DIR/compose-images.txt" compose config --images
	grep -Fxq -- "$IMAGE_REF" "$EVIDENCE_DIR/compose-images.txt" || die "Compose did not select the proven Dorf image"
	if grep '^ghcr.io/aphronio/dorf:' "$EVIDENCE_DIR/compose-images.txt" | grep -Fvx -- "$IMAGE_REF" >/dev/null; then
		die "Compose selected a different Dorf image"
	fi
	rm -f -- "$SECRET_FILE"
	SECRET_FILE=
	EPHEMERAL_KEY_PATH=
	capture "$EVIDENCE_DIR/provider-status.json" "$DORF_BIN" provider status \
		--profile "$PROFILE_NAME" --ai-connection "$CONNECTION_NAME" --json
}

capture_compose_status() {
	local destination=$1 service
	capture "$destination" compose ps --all --format json
	for service in postgres worker control-api; do
		one_service_container "$service" >/dev/null
	done
}

one_service_container() {
	local service=$1
	local ids
	ids=$(compose ps --quiet "$service")
	[[ "$ids" != *$'\n'* && -n "$ids" ]] || die "expected exactly one running $service container"
	printf '%s\n' "$ids"
}

assert_compose_runtime() {
	local postgres worker api migrate receipt="$EVIDENCE_DIR/compose-runtime.txt"
	postgres=$(one_service_container postgres)
	worker=$(one_service_container worker)
	api=$(one_service_container control-api)
	local postgres_inspection="$WORK_ROOT/docker-inspect-$postgres.json"
	docker inspect "$postgres" >"$postgres_inspection"
	jq -e 'any((.[0].NetworkSettings.Ports["5432/tcp"] // [])[]; .HostIp == "127.0.0.1" and .HostPort == "54329")' "$postgres_inspection" >/dev/null ||
		die "PostgreSQL is not published on host loopback"
	local worker_inspection="$WORK_ROOT/docker-inspect-$worker.json"
	docker inspect "$worker" >"$worker_inspection"
	jq -e '.[0].State.Health.Status == "healthy"' "$worker_inspection" >/dev/null ||
		die "worker is not healthy"
	local api_inspection="$WORK_ROOT/docker-inspect-$api.json"
	docker inspect "$api" >"$api_inspection"
	jq -e '.[0].State.Health.Status == "healthy"' "$api_inspection" >/dev/null ||
		die "control API is not healthy"
	jq -e 'any((.[0].NetworkSettings.Ports["8745/tcp"] // [])[]; .HostPort == "8745" and (.HostIp == "0.0.0.0" or .HostIp == "::"))' "$api_inspection" >/dev/null ||
		die "control API is not published on host port 8745"
	jq -e '[.[0].Config.Env[]? | select(startswith("E2B_API_KEY=") or startswith("DORF_INCUS_") or startswith("DORF_PROVIDER_GATEWAY_") or startswith("DORF_CONFIG_DIR=") or startswith("DORF_DATA_DIR="))] | length == 0' "$api_inspection" >/dev/null ||
		die "control API received provider environment authority"
	jq -e '[.[0].Mounts[]? | select(.Destination == "/var/lib/dorf/.config/dorf" or .Destination == "/var/lib/dorf/.local/share/dorf")] | length == 0' "$api_inspection" >/dev/null ||
		die "control API received provider configuration or data custody"
	[[ -z "$(docker ps -aq --filter label=com.docker.compose.service=control-reader)" ]] ||
		die "obsolete standalone control-reader container is running"
	migrate=$(compose ps --all --quiet migrate)
	[[ -n "$migrate" && "$migrate" != *$'\n'* ]] || die "expected exactly one completed migrate container"
	local migrate_inspection="$WORK_ROOT/docker-inspect-$migrate.json"
	docker inspect "$migrate" >"$migrate_inspection"
	jq -e '.[0].State.Status == "exited" and .[0].State.ExitCode == 0' "$migrate_inspection" >/dev/null ||
		die "one-shot migration did not complete successfully"
	printf 'migrate=complete\npostgres=host-loopback\nworker=healthy\napi=healthy-host-port-provider-authority-absent\nreader=worker-hosted\n' >"$receipt"
	chmod 0600 "$receipt"
}

proof_nonce() {
	local nonce=${DORF_PROOF_NONCE:-}
	if [[ -z "$nonce" ]]; then
		nonce=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
	fi
	[[ "$nonce" =~ ^[0-9a-f]{32}$ ]] || die "proof nonce must be exactly 32 lowercase hexadecimal characters"
	printf '%s\n' "$nonce"
}

wait_for_job_completion() {
	local attempts=${DORF_PROOF_WAIT_ATTEMPTS:-180}
	local delay=${DORF_PROOF_POLL_SECONDS:-2}
	local snapshot="$EVIDENCE_DIR/job-inspect.json"
	local before_restart="$EVIDENCE_DIR/job-inspect-before-worker-restart.json"
	local restarted=0 attempt current active_run_id worker_container worker_started_before worker_started_after
	for ((attempt = 1; attempt <= attempts; attempt++)); do
		capture "$snapshot" "$DORF_BIN" inspect --json "$JOB_ID"
		if [[ -z "$SANDBOX_ID" ]]; then
			SANDBOX_ID=$(jq -r 'first(.observed_facts.sandboxes[]? | select(.name == "default") | .id) // ""' "$snapshot")
		fi
		if [[ -n "$SANDBOX_ID" && "$restarted" -eq 0 ]]; then
			active_run_id=$(jq -r --arg sid "$SANDBOX_ID" \
				'first(.observed_facts.agent_runs[]? | select(.state == "active" and .sandbox_id == $sid and (.id // "") != "") | .id) // ""' \
				"$snapshot")
			if [[ -z "$active_run_id" ]]; then
				sleep "$delay"
				continue
			fi
			install -m 0600 "$snapshot" "$before_restart"
			worker_container=$(one_service_container worker)
			worker_started_before=$(docker inspect --format '{{.State.StartedAt}}' "$worker_container")
			compose restart worker >/dev/null
			wait_for_service_health worker
			capture_compose_status "$EVIDENCE_DIR/compose-status-after-worker-restart.json"
			worker_container=$(one_service_container worker)
			worker_started_after=$(docker inspect --format '{{.State.StartedAt}}' "$worker_container")
			[[ "$worker_started_after" != "$worker_started_before" ]] || die "worker start time did not change across restart"
			restarted=1
			continue
		fi
		current=$(jq -r '.current // ""' "$snapshot")
		if [[ "$current" == "Open and idle" ]] && [[ -n "$active_run_id" ]] && jq -e --arg rid "$active_run_id" \
			'any(.observed_facts.agent_runs[]?; .id == $rid and .state == "completed" and .turn_outcome == "completed")' \
			"$snapshot" >/dev/null; then
			[[ "$restarted" -eq 1 ]] || die "Job completed without proving worker restart during Sandbox custody"
			printf 'JOB -> Sandbox custody -> worker restart -> Open and idle\n'
			return
		fi
		[[ "$current" != "Needs attention" ]] || die "Job entered Needs attention"
		sleep "$delay"
	done
	die "Job did not reach Open and idle with a completed Turn"
}

wait_for_service_health() {
	local service=$1 attempts=${DORF_PROOF_WAIT_ATTEMPTS:-90} delay=${DORF_PROOF_POLL_SECONDS:-2}
	local attempt container
	for ((attempt = 1; attempt <= attempts; attempt++)); do
		container=$(compose ps --quiet "$service")
		if [[ -n "$container" ]] && [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container")" == healthy ]]; then
			return 0
		fi
		sleep "$delay"
	done
	die "$service did not become healthy"
}

wait_for_cleanup() {
	local attempts=${DORF_PROOF_WAIT_ATTEMPTS:-180}
	local delay=${DORF_PROOF_POLL_SECONDS:-2}
	local snapshot="$EVIDENCE_DIR/cleanup-inspect.json"
	local attempt
	for ((attempt = 1; attempt <= attempts; attempt++)); do
		capture "$snapshot" "$DORF_BIN" inspect --json "$JOB_ID"
		if jq -e --arg sid "$SANDBOX_ID" \
			'.job.cleanup_state == "complete" and any(.observed_facts.actions[]?; .kind == "sandbox-delete" and .scope == $sid and .state == "succeeded")' \
			"$snapshot" >/dev/null; then
			return
		fi
		sleep "$delay"
	done
	die "cleanup did not durably complete the exact Sandbox deletion"
}

prove() {
	arm_ephemeral_key_cleanup "$@"
	parse_proof_options "$@"
	for command in docker incus jq od realpath sha256sum tar; do
		require_command "$command"
	done
	require_nested_kvm
	docker info >/dev/null || die "proof operator cannot reach the local Docker daemon"
	docker compose version >/dev/null || die "Docker Compose is unavailable to the proof operator"
	incus --force-local --project dorf query /1.0 >/dev/null ||
		die "proof operator cannot reach the restricted local Incus project"
	[[ -r "$KVM_DEVICE" && -w "$KVM_DEVICE" ]] || die "proof operator cannot open the KVM device"
	assert_fresh_cache
	prepare_release
	run_setup
	capture_compose_status "$EVIDENCE_DIR/compose-status.json"
	assert_compose_runtime

	local nonce goal_file admission expected observed
	nonce=$(proof_nonce)
	expected="$WORK_ROOT/expected-PROOF.txt"
	observed="$WORK_ROOT/observed-PROOF.txt"
	printf '%s\n' "$nonce" >"$expected"
	chmod 0600 "$expected"
	goal_file="$WORK_ROOT/goal.txt"
	printf '%s\n' \
		"Create /workspace/job/PROOF.txt with exactly this one line, including its trailing newline:" \
		"$nonce" \
		"Do not modify the line. Finish after reading the file back and confirming it." >"$goal_file"
	admission="$EVIDENCE_DIR/job-admission.json"
	capture "$admission" "$DORF_BIN" run \
		--key "compose-vm-proof-$nonce" \
		--goal-file "$goal_file" \
		--profile "$PROFILE_NAME" \
		--ai-connection "$CONNECTION_NAME" \
		--reasoning high
	JOB_ID=$(jq -er '.job_id' "$admission")
	jq -e '.scheduled == true' "$admission" >/dev/null || die "direct Job was not durably scheduled"
	wait_for_job_completion
	[[ "$SANDBOX_ID" =~ ^[A-Za-z0-9._-]+$ ]] || die "Job exposed an unsafe Sandbox identity"

	compose restart control-api >/dev/null
	wait_for_service_health control-api
	capture_compose_status "$EVIDENCE_DIR/compose-status-after-api-restart.json"
	"$DORF_BIN" sandbox file get "$SANDBOX_ID" PROOF.txt --output "$observed"
	cmp "$expected" "$observed" || die "PROOF.txt bytes differ from the admitted nonce"
	capture "$EVIDENCE_DIR/cleanup-request.json" "$DORF_BIN" cleanup "$JOB_ID"
	wait_for_cleanup
	incus --force-local --project dorf query /1.0 >/dev/null
	capture "$EVIDENCE_DIR/incus-after-cleanup.json" incus --force-local --project dorf list --format json
	jq -e --arg sid "$SANDBOX_ID" 'all(.[]; .name != $sid)' "$EVIDENCE_DIR/incus-after-cleanup.json" >/dev/null ||
		die "inner Incus still contains the cleaned Sandbox instance"

	PROOF_COMPLETE=1
	printf 'FILE -> exact nonce bytes -> Compose API restart -> cleanup -> Sandbox absent\n'
}

main() {
	[[ $# -ge 1 ]] || { usage >&2; exit 2; }
	local phase=$1
	shift
	case "$phase" in
	cache-prep) cache_prep "$@" ;;
	prove) prove "$@" ;;
	-h | --help) usage ;;
	*) usage >&2; die "unknown phase '$phase'" ;;
	esac
}

main "$@"
