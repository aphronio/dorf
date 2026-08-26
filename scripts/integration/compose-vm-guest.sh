#!/usr/bin/env bash
set -euo pipefail

readonly KVM_DEVICE="${DORF_PROOF_KVM_DEVICE:-/dev/kvm}"
readonly CPUINFO="${DORF_PROOF_CPUINFO:-/proc/cpuinfo}"
readonly PROFILE_NAME="compose-vm-proof"
readonly CONNECTION_NAME="openai-api"
readonly MAX_EVIDENCE_BYTES=262144

DORF_BIN=
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
  compose-vm-guest.sh prove [proof options]

cache-prep is an explicit administrator phase. prove refuses root and exercises
only Dorf's public CLI plus read-only runtime observations.
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
	[[ "$user" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "cache-prep requires one safe ordinary user"
	[[ "$(id -u "$user")" -ne 0 ]] || die "cache-prep user must be ordinary"
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

	id -nG "$user" | tr ' ' '\n' | grep -Fx docker >/dev/null || die "$user did not receive Docker authority"
	id -nG "$user" | tr ' ' '\n' | grep -Fx incus-admin >/dev/null || die "$user did not receive Incus authority"
	id -nG "$user" | tr ' ' '\n' | grep -Fx kvm >/dev/null || die "$user did not receive KVM access"
	as_user "$user" test -r "$KVM_DEVICE"
	as_user "$user" test -w "$KVM_DEVICE"
	as_user "$user" docker info >/dev/null
	as_user "$user" docker compose version >/dev/null

	local docker_containers docker_images docker_volumes incus_instances incus_images
	docker_containers=$(as_user "$user" docker ps -aq) || die "$user cannot inventory Docker containers"
	docker_images=$(as_user "$user" docker image ls -q) || die "$user cannot inventory Docker images"
	docker_volumes=$(as_user "$user" docker volume ls -q) || die "$user cannot inventory Docker volumes"
	incus_instances=$(as_user "$user" incus --force-local --project dorf list --format csv -c n) || die "$user cannot inventory the restricted Incus project"
	incus_images=$(as_user "$user" incus --force-local --project dorf image list --format csv -c f) || die "$user cannot inventory restricted Incus images"
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
	if [[ -n "$DORF_BIN" && -x "$DORF_BIN" ]]; then
		capture "$EVIDENCE_DIR/failure-service-status.json" "$DORF_BIN" service status --output json || true
		capture "$EVIDENCE_DIR/failure-api.log" "$DORF_BIN" service logs api --lines 200 || true
		capture "$EVIDENCE_DIR/failure-worker.log" "$DORF_BIN" service logs worker --lines 200 || true
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
	CONTAINER_IMAGE=
	CHECKSUMS=
	SANDBOX_ARCHIVE=
	SANDBOX_MANIFEST=
	SECRET_FILE=
	WORK_ROOT=
	EVIDENCE_DIR=
	local openai_key_count=0
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--app-archive) [[ $# -ge 2 ]] || die "--app-archive needs a value"; APP_ARCHIVE=$2; shift 2 ;;
		--container-image) [[ $# -ge 2 ]] || die "--container-image needs a value"; CONTAINER_IMAGE=$2; shift 2 ;;
		--checksums) [[ $# -ge 2 ]] || die "--checksums needs a value"; CHECKSUMS=$2; shift 2 ;;
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
	local evidence_arg=$EVIDENCE_DIR
	EVIDENCE_DIR=
	SECRET_FILE=$(canonical_input_file "$SECRET_FILE" "--openai-key-file")
	[[ "$(stat -c '%a' "$SECRET_FILE")" == 600 && "$(stat -c '%u' "$SECRET_FILE")" == "$(id -u)" ]] ||
		die "ephemeral key file must be operator-owned mode 0600"
	trap cleanup_proof EXIT
	EPHEMERAL_KEY_PATH=$SECRET_FILE
	APP_ARCHIVE=$(canonical_input_file "$APP_ARCHIVE" "--app-archive")
	CONTAINER_IMAGE=$(canonical_input_file "$CONTAINER_IMAGE" "--container-image")
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
	containers=$(docker ps -aq) || die "ordinary proof user cannot inventory Docker containers"
	images=$(docker image ls -q) || die "ordinary proof user cannot inventory Docker images"
	volumes=$(docker volume ls -q) || die "ordinary proof user cannot inventory Docker volumes"
	[[ -z "$containers" ]] || die "fresh proof VM already contains Docker containers"
	[[ -z "$images" ]] || die "fresh proof VM already contains Docker images"
	[[ -z "$volumes" ]] || die "fresh proof VM already contains Docker volumes"
	incus --force-local --project dorf query /1.0 >/dev/null
	incus --force-local --project dorf list --format json | jq -e 'length == 0' >/dev/null || die "fresh proof VM contains an Incus instance"
	incus --force-local --project dorf image list --format json | jq -e 'length == 0' >/dev/null || die "fresh proof VM contains a Dorf project image"
	for path in "$HOME/.config/dorf" "$HOME/.local/share/dorf" "$HOME/.local/state/dorf"; do
		[[ ! -e "$path" && ! -L "$path" ]] || die "fresh proof VM contains Dorf state at $path"
	done
	if command -v dorf >/dev/null 2>&1; then
		die "fresh proof VM contains an installed Dorf binary"
	fi
}

prepare_release() {
	local app_base image_base checksums_base version
	app_base=$(basename -- "$APP_ARCHIVE")
	[[ "$app_base" =~ ^dorf_([0-9]+\.[0-9]+\.[0-9]+)_linux_x86_64\.tar\.gz$ ]] || die "application archive name is invalid"
	version=${BASH_REMATCH[1]}
	image_base=$(basename -- "$CONTAINER_IMAGE")
	checksums_base=$(basename -- "$CHECKSUMS")
	[[ "$image_base" == "dorf_${version}_linux_x86_64_container-image.docker.tar" ]] || die "container image release differs from application release"
	[[ "$checksums_base" == "dorf_${version}_checksums.txt" ]] || die "checksum release differs from application release"
	[[ "$(basename -- "$SANDBOX_ARCHIVE")" == dorf-incus-vm-v5-x86_64.tar.gz ]] || die "Sandbox archive name is invalid"
	[[ "$(basename -- "$SANDBOX_MANIFEST")" == dorf-incus-vm-v5-x86_64.json ]] || die "Sandbox manifest name is invalid"
	local staging="$WORK_ROOT/release-inputs"
	install -d -m 0700 "$staging"
	cp -- "$APP_ARCHIVE" "$CONTAINER_IMAGE" "$CHECKSUMS" "$staging/"
	[[ "$(wc -l <"$staging/$checksums_base")" -eq 2 ]] || die "release checksum authority must contain exactly two entries"
	[[ "$(awk -v file="$app_base" '$2 == file {n++} END {print n + 0}' "$staging/$checksums_base")" -eq 1 ]] ||
		die "release checksums do not name the application archive exactly once"
	[[ "$(awk -v file="$image_base" '$2 == file {n++} END {print n + 0}' "$staging/$checksums_base")" -eq 1 ]] ||
		die "release checksums do not name the container image exactly once"
	(
		cd -- "$staging"
		sha256sum --check --strict "$checksums_base" >/dev/null
	)
	install -d -m 0700 "$WORK_ROOT/release"
	tar -xzf "$staging/$app_base" -C "$WORK_ROOT/release"
	DORF_BIN="$WORK_ROOT/release/dorf"
	[[ -f "$DORF_BIN" && ! -L "$DORF_BIN" && -x "$DORF_BIN" ]] || die "application archive contains no executable Dorf binary"
	[[ "$("$DORF_BIN" version)" == "dorf $version" ]] || die "extracted binary version differs from the release asset"
	docker load --input "$staging/$image_base" >/dev/null
	LOCAL_IMAGE="ghcr.io/aphronio/dorf:$version"
	local image_id
	image_id=$(docker image inspect --format '{{.Id}}' "$LOCAL_IMAGE")
	[[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || die "loaded container image has no immutable Docker identity"
}

# This is the single guided-setup seam. It must consume only the injected,
# checksummed application, container image, and official Incus image assets.
run_setup() {
	capture "$EVIDENCE_DIR/setup.log" "$DORF_BIN" setup \
		--yes \
		--local-image "$LOCAL_IMAGE" \
		--sandbox-provider incus \
		--profile "$PROFILE_NAME" \
		--connection-auth openai \
		--openai-api-key-file "$SECRET_FILE" \
		--incus-manifest "$SANDBOX_MANIFEST" \
		--incus-archive "$SANDBOX_ARCHIVE"
	rm -f -- "$SECRET_FILE"
	SECRET_FILE=
	EPHEMERAL_KEY_PATH=
	capture "$EVIDENCE_DIR/provider-status.json" "$DORF_BIN" provider status \
		--profile "$PROFILE_NAME" --ai-connection "$CONNECTION_NAME" --json
}

capture_ready_status() {
	local destination=$1
	capture "$destination" "$DORF_BIN" service status --output json
	jq -e '.ready == true' "$destination" >/dev/null || die "Compose service status is not ready"
}

one_service_container() {
	local service=$1
	local ids
	ids=$(docker ps --filter "label=com.docker.compose.service=$service" --format '{{.ID}}')
	[[ "$ids" != *$'\n'* && -n "$ids" ]] || die "expected exactly one running $service container"
	printf '%s\n' "$ids"
}

assert_compose_topology() {
	local worker reader api postgres
	worker=$(one_service_container worker)
	reader=$(one_service_container control-reader)
	api=$(one_service_container control-api)
	postgres=$(one_service_container postgres)
	local ids id receipt="$EVIDENCE_DIR/compose-topology.txt"
	ids=$(docker ps --filter label=com.docker.compose.service --format '{{.ID}}')
	[[ -n "$ids" ]] || die "Compose exposed no managed service containers"
	: >"$receipt"
	while IFS= read -r id; do
		[[ -n "$id" ]] || continue
		[[ "$id" =~ ^[A-Za-z0-9._-]+$ ]] || die "Docker exposed an unsafe container identity"
		local inspection="$WORK_ROOT/docker-inspect-$id.json"
		docker inspect "$id" >"$inspection"
		jq -e '.[0].HostConfig.NetworkMode != "host"' "$inspection" >/dev/null || die "$id uses host networking"
		jq -e '[.[0].Mounts[]? | select((.Source // "" | endswith("/docker.sock")) or (.Destination // "" | endswith("/docker.sock")))] | length == 0' "$inspection" >/dev/null ||
			die "$id mounts the Docker socket"
		printf '%s bridge-no-docker-socket\n' "$id" >>"$receipt"
	done <<<"$ids"
	for id in "$api" "$postgres"; do
		local inspection="$WORK_ROOT/docker-inspect-$id.json"
		jq -e '([.[0].NetworkSettings.Ports | to_entries[]?.value[]?.HostIp] as $ips | ($ips | length) > 0 and all($ips[]; . == "127.0.0.1"))' "$inspection" >/dev/null ||
			die "$id does not publish only on loopback"
	done
	local reader_inspection="$WORK_ROOT/docker-inspect-$reader.json"
	jq -e '.[0].State.Health.Status == "healthy"' "$reader_inspection" >/dev/null ||
		die "control-reader did not prove its authenticated health operation"
	jq -e '([.[0].NetworkSettings.Networks | keys[] | sub("^dorf_"; "")] | sort) == (["database", "provider", "reader", "reader-egress"] | sort)' "$reader_inspection" >/dev/null ||
		die "control-reader network custody differs from the reviewed topology"
	local api_inspection="$WORK_ROOT/docker-inspect-$api.json"
	jq -e '([.[0].NetworkSettings.Networks | keys[] | sub("^dorf_"; "")] | sort) == (["database", "reader"] | sort)' "$api_inspection" >/dev/null ||
		die "control API network custody differs from the reviewed topology"
	jq -e '[.[0].Config.Env[]? | select(startswith("E2B_API_KEY=") or startswith("DORF_INCUS_") or startswith("DORF_PROVIDER_GATEWAY_") or startswith("DORF_CONFIG_DIR=") or startswith("DORF_DATA_DIR="))] | length == 0' "$api_inspection" >/dev/null ||
		die "control API received provider environment authority"
	jq -e '([.[0].Mounts[]?.Destination] | sort) == (["/var/lib/dorf/.config", "/var/lib/dorf/.local/state/dorf"] | sort)' "$api_inspection" >/dev/null ||
		die "control API mounts exceed sanitized configuration and read-only state"
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
	local restarted=0 attempt current active_run_id worker_before worker_after
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
			worker_before=$(one_service_container worker)
			"$DORF_BIN" service restart worker >/dev/null
			capture_ready_status "$EVIDENCE_DIR/service-status-after-worker-restart.json"
			worker_after=$(one_service_container worker)
			[[ "$worker_after" != "$worker_before" ]] || die "worker container identity did not change across restart"
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
	[[ "$(id -u)" -ne 0 ]] || die "prove must run as an ordinary user"
	arm_ephemeral_key_cleanup "$@"
	for command in docker incus jq od realpath sha256sum tar; do
		require_command "$command"
	done
	require_nested_kvm
	id -nG | tr ' ' '\n' | grep -Fx docker >/dev/null || die "ordinary proof user lacks Docker authority"
	id -nG | tr ' ' '\n' | grep -Fx incus-admin >/dev/null || die "ordinary proof user lacks Incus authority"
	id -nG | tr ' ' '\n' | grep -Fx kvm >/dev/null || die "ordinary proof user lacks KVM device access"
	[[ -r "$KVM_DEVICE" && -w "$KVM_DEVICE" ]] || die "ordinary proof user cannot open the KVM device"
	parse_proof_options "$@"
	assert_fresh_cache
	prepare_release
	run_setup
	capture_ready_status "$EVIDENCE_DIR/service-status.json"
	assert_compose_topology

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

	"$DORF_BIN" service restart api >/dev/null
	capture_ready_status "$EVIDENCE_DIR/service-status-after-api-restart.json"
	"$DORF_BIN" sandbox file get "$SANDBOX_ID" PROOF.txt --output "$observed"
	cmp "$expected" "$observed" || die "PROOF.txt bytes differ from the admitted nonce"
	capture "$EVIDENCE_DIR/cleanup-request.json" "$DORF_BIN" cleanup "$JOB_ID"
	wait_for_cleanup
	incus --force-local --project dorf query /1.0 >/dev/null
	capture "$EVIDENCE_DIR/incus-after-cleanup.json" incus --force-local --project dorf list --format json
	jq -e --arg sid "$SANDBOX_ID" 'all(.[]; .name != $sid)' "$EVIDENCE_DIR/incus-after-cleanup.json" >/dev/null ||
		die "inner Incus still contains the cleaned Sandbox instance"

	PROOF_COMPLETE=1
	printf 'FILE -> exact nonce bytes -> API restart -> cleanup -> Sandbox absent\n'
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
