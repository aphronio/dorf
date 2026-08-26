#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_ROOT="$(cd -- "$SCRIPT_DIR/../.." && pwd)"
readonly HARNESS_SCHEMA="1"
readonly PROOF_OWNER="dorf-compose-vm-proof-v${HARNESS_SCHEMA}"
readonly PROOF_PROJECT="dorf-compose-vm-proof-v${HARNESS_SCHEMA}"
readonly UBUNTU_BASE="images:ubuntu/24.04/cloud"
readonly GUEST_USER="ubuntu"
readonly KVM_DEVICE="${DORF_PROOF_KVM_DEVICE:-/dev/kvm}"
readonly INCUS_BIN="${INCUS_BIN:-incus}"
readonly DOCKER_HELPER="$PROJECT_ROOT/scripts/bootstrap/docker.sh"
readonly INCUS_HELPER="$PROJECT_ROOT/scripts/bootstrap/incus.sh"
readonly GUEST_SCRIPT="$SCRIPT_DIR/compose-vm-guest.sh"
readonly MAX_EVIDENCE_FILE_BYTES=262144
readonly MAX_EVIDENCE_TOTAL_BYTES=4194304
readonly -a REMOTE_EVIDENCE_FILES=(
	setup.log compose-images.txt provider-status.json
	compose-status.json compose-status-after-worker-restart.json compose-status-after-api-restart.json
	compose-runtime.txt job-admission.json job-inspect-before-worker-restart.json job-inspect.json
	cleanup-request.json cleanup-inspect.json incus-after-cleanup.json
	failure-compose-status.json failure-api.log failure-worker.log failure-inspect.json failure-incus.json
)

ACTIVE_INSTANCE=
ACTIVE_ROLE=
ACTIVE_CACHE_KEY=
ACTIVE_RUN_ID=
ACTIVE_SECRET_PATH=
ACTIVE_EVIDENCE_SOURCE=
ACTIVE_EVIDENCE_DIR=
EPHEMERAL_SECRET=
TEMP_ROOT=

usage() {
	cat <<'EOF'
Usage:
  scripts/integration/compose-vm.sh refresh-cache
  scripts/integration/compose-vm.sh prove --openai-key-file PATH \
	--app-archive PATH --checksums PATH --image-ref REF \
	--sandbox-archive PATH --sandbox-manifest PATH [--evidence-root PATH]

refresh-cache is the explicit administrator phase. It freezes Docker, Compose,
and an empty restricted local Incus project into a keyed Ubuntu 24.04 VM image.

prove launches one disposable VM from that cache and runs Dorf as the prepared
operator. It never installs host dependencies or reuses Dorf state.
EOF
}

die() {
	printf 'compose-vm: %s\n' "$1" >&2
	exit 1
}

require_host_virtualization() {
	if [[ ! -c "$KVM_DEVICE" ]]; then
		die "nested virtualization is unavailable: $KVM_DEVICE is not a KVM character device"
	fi
	local state=${DORF_PROOF_NESTED_KVM_STATE:-}
	if [[ -n "$state" ]]; then
		[[ -r "$state" ]] || die "nested virtualization is unavailable: cannot read $state"
		grep -Eq '^(Y|1)$' "$state" || die "nested virtualization is disabled according to $state"
		return
	fi
	for state in /sys/module/kvm_intel/parameters/nested /sys/module/kvm_amd/parameters/nested; do
		if [[ -r "$state" ]] && grep -Eq '^(Y|1)$' "$state"; then
			return
		fi
	done
	die "nested virtualization is unavailable: enable the host KVM nested module parameter"
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command is unavailable: $1"
}

outer_incus() {
	"$INCUS_BIN" --force-local --project "$PROOF_PROJECT" "$@"
}

proof_project_exists() {
	"$INCUS_BIN" --force-local project show "$PROOF_PROJECT" >/dev/null 2>&1
}

assert_proof_project() {
	proof_project_exists || die "dedicated local Incus project $PROOF_PROJECT is absent; run refresh-cache explicitly"
	local key expected
	while IFS=$'\t' read -r key expected; do
		[[ "$("$INCUS_BIN" --force-local project get "$PROOF_PROJECT" "$key")" == "$expected" ]] ||
			die "refusing Incus project $PROOF_PROJECT: ownership or project shape differs"
	done <<EOF
user.dorf.proof.owner	$PROOF_OWNER
features.images	true
features.profiles	false
EOF
}

ensure_proof_project() {
	if ! proof_project_exists; then
		"$INCUS_BIN" --force-local project create "$PROOF_PROJECT" \
			-c "user.dorf.proof.owner=$PROOF_OWNER" \
			-c features.images=true \
			-c features.profiles=false
	fi
	assert_proof_project
}

random_id() {
	local value=${DORF_PROOF_RUN_ID:-}
	if [[ -z "$value" ]]; then
		value=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
	fi
	[[ "$value" =~ ^[0-9a-f]{12}$ ]] || die "proof run identity must be exactly 12 lowercase hexadecimal characters"
	printf '%s\n' "$value"
}

resolve_base_fingerprint() {
	local output fingerprint
	output=$(outer_incus image info "$UBUNTU_BASE" --vm) || die "cannot resolve the Ubuntu 24.04 VM base image"
	fingerprint=$(sed -n 's/^Fingerprint: //p' <<<"$output")
	[[ "$fingerprint" =~ ^[0-9a-f]{64}$ ]] || die "Ubuntu 24.04 VM base did not resolve to one immutable fingerprint"
	printf '%s\n' "$fingerprint"
}

set_cache_identity() {
	local docker_sha=$1 incus_sha=$2
	[[ -f "$GUEST_SCRIPT" && ! -L "$GUEST_SCRIPT" ]] || die "compose VM guest harness is unavailable"
	BASE_FINGERPRINT=$(resolve_base_fingerprint)
	DOCKER_SHA=$docker_sha
	INCUS_SHA=$incus_sha
	GUEST_SHA=$(sha256sum "$GUEST_SCRIPT" | awk '{print $1}')
	CACHE_KEY=$(printf 'schema=%s\nbase=%s\ndocker=%s\nincus=%s\nguest=%s\n' \
		"$HARNESS_SCHEMA" "$BASE_FINGERPRINT" "$DOCKER_SHA" "$INCUS_SHA" "$GUEST_SHA" | sha256sum | awk '{print $1}')
	CACHE_ALIAS="dorf-proof-cache-v${HARNESS_SCHEMA}-${CACHE_KEY:0:20}"
	[[ "$CACHE_ALIAS" =~ ^dorf-proof-cache-v[0-9]+-[0-9a-f]{20}$ ]] || die "generated cache alias is unsafe"
}

prepare_cache_identity() {
	[[ -f "$DOCKER_HELPER" && ! -L "$DOCKER_HELPER" ]] || die "canonical docker.sh helper is unavailable"
	[[ -f "$INCUS_HELPER" && ! -L "$INCUS_HELPER" ]] || die "canonical incus.sh helper is unavailable"
	set_cache_identity \
		"$(sha256sum "$DOCKER_HELPER" | awk '{print $1}')" \
		"$(sha256sum "$INCUS_HELPER" | awk '{print $1}')"
}

image_exists() {
	outer_incus image info "$1" >/dev/null 2>&1
}

image_property() {
	outer_incus image get-property "$1" "$2"
}

assert_cache_image() {
	local alias=$1
	local key value
	while IFS=$'\t' read -r key value; do
		[[ "$(image_property "$alias" "$key")" == "$value" ]] ||
			die "refusing cache alias $alias: ownership or cache identity does not match"
	done <<EOF
dorf.proof.owner	$PROOF_OWNER
dorf.proof.schema	$HARNESS_SCHEMA
dorf.proof.cache_key	$CACHE_KEY
dorf.proof.base_fingerprint	$BASE_FINGERPRINT
dorf.proof.docker_sha256	$DOCKER_SHA
dorf.proof.incus_sha256	$INCUS_SHA
dorf.proof.guest_sha256	$GUEST_SHA
EOF
}

instance_exists() {
	outer_incus info "$1" >/dev/null 2>&1
}

assert_owned_instance() {
	local instance=$1
	local role=$2
	local cache_key=$3
	local run_id=$4
	[[ "$instance" =~ ^dorf-proof-(cache|run)-[0-9a-f]{12}$ ]] || {
		printf 'compose-vm: refusing unsafe proof instance name %s\n' "$instance" >&2
		return 1
	}
	[[ "$(outer_incus config get "$instance" user.dorf.proof.owner)" == "$PROOF_OWNER" ]] ||
		{ printf 'compose-vm: refusing to manage %s: ownership label differs\n' "$instance" >&2; return 1; }
	[[ "$(outer_incus config get "$instance" user.dorf.proof.role)" == "$role" ]] ||
		{ printf 'compose-vm: refusing to manage %s: role label differs\n' "$instance" >&2; return 1; }
	[[ "$(outer_incus config get "$instance" user.dorf.proof.cache_key)" == "$cache_key" ]] ||
		{ printf 'compose-vm: refusing to manage %s: cache identity differs\n' "$instance" >&2; return 1; }
	[[ "$(outer_incus config get "$instance" user.dorf.proof.run_id)" == "$run_id" ]] ||
		{ printf 'compose-vm: refusing to manage %s: run identity differs\n' "$instance" >&2; return 1; }
}

delete_owned_instance() {
	local instance=$1 role=$2 cache_key=$3 run_id=$4
	instance_exists "$instance" || return 0
	assert_owned_instance "$instance" "$role" "$cache_key" "$run_id"
	outer_incus delete "$instance" --force
}

cleanup_active_instance() {
	local status=$?
	trap - EXIT
	if [[ -n "$ACTIVE_INSTANCE" ]] && instance_exists "$ACTIVE_INSTANCE"; then
		if assert_owned_instance "$ACTIVE_INSTANCE" "$ACTIVE_ROLE" "$ACTIVE_CACHE_KEY" "$ACTIVE_RUN_ID"; then
			if [[ -n "$ACTIVE_SECRET_PATH" ]]; then
				outer_incus exec "$ACTIVE_INSTANCE" -- rm -f "$ACTIVE_SECRET_PATH" >/dev/null 2>&1 || status=1
			fi
			pull_active_evidence || true
			if ! delete_owned_instance "$ACTIVE_INSTANCE" "$ACTIVE_ROLE" "$ACTIVE_CACHE_KEY" "$ACTIVE_RUN_ID"; then
				status=1
			fi
		else
			printf 'compose-vm: could not remove attested disposable instance %s\n' "$ACTIVE_INSTANCE" >&2
			status=1
		fi
	fi
	cleanup_temp_root
	exit "$status"
}

cleanup_temp_root() {
	if [[ -n "$TEMP_ROOT" ]]; then
		case "$TEMP_ROOT" in
		/tmp/dorf-compose-vm.*) rm -rf -- "$TEMP_ROOT" ;;
		*) printf 'compose-vm: refusing unsafe temporary cleanup path %s\n' "$TEMP_ROOT" >&2 ;;
		esac
		TEMP_ROOT=
	fi
}

sanitize_evidence() {
	local directory=$1
	[[ -d "$directory" && ! -L "$directory" ]] || return 0
	local name file size total=0
	for name in "${REMOTE_EVIDENCE_FILES[@]}"; do
		file="$directory/$name"
		[[ -f "$file" && ! -L "$file" ]] || continue
		size=$(stat -c '%s' "$file" 2>/dev/null || printf '0')
		if ((size > MAX_EVIDENCE_FILE_BYTES)); then
			printf '[discarded: evidence exceeded 256 KiB file bound]\n' >"$file"
		elif [[ -n "$EPHEMERAL_SECRET" ]] && grep -Fq -- "$EPHEMERAL_SECRET" "$file"; then
			printf '[redacted: ephemeral secret detected]\n' >"$file"
		fi
		size=$(stat -c '%s' "$file" 2>/dev/null || printf '0')
		if ((total + size > MAX_EVIDENCE_TOTAL_BYTES)); then
			printf '[discarded: evidence exceeded 4 MiB aggregate bound]\n' >"$file"
			size=$(stat -c '%s' "$file")
		fi
		total=$((total + size))
	done
}

pull_active_evidence() {
	[[ -n "$ACTIVE_INSTANCE" && -n "$ACTIVE_EVIDENCE_SOURCE" && -n "$ACTIVE_EVIDENCE_DIR" ]] || return 0
	mkdir -p "$ACTIVE_EVIDENCE_DIR"
	local name
	for name in "${REMOTE_EVIDENCE_FILES[@]}"; do
		outer_incus file pull \
			"$ACTIVE_INSTANCE$ACTIVE_EVIDENCE_SOURCE/$name" "$ACTIVE_EVIDENCE_DIR/$name" >/dev/null 2>&1 || true
	done
	sanitize_evidence "$ACTIVE_EVIDENCE_DIR"
}

wait_for_guest() {
	local instance=$1
	local attempts=${DORF_PROOF_WAIT_ATTEMPTS:-90}
	local delay=${DORF_PROOF_POLL_SECONDS:-2}
	local attempt
	for ((attempt = 1; attempt <= attempts; attempt++)); do
		if outer_incus exec "$instance" -- true >/dev/null 2>&1; then
			return 0
		fi
		sleep "$delay"
	done
	outer_incus exec "$instance" -- true >/dev/null
}

refresh_cache() {
	require_command "$INCUS_BIN"
	require_command sha256sum
	require_command od
	ensure_proof_project
	prepare_cache_identity
	if image_exists "$CACHE_ALIAS"; then
		assert_cache_image "$CACHE_ALIAS"
		printf 'HOST ADMIN -> frozen cache already ready: %s\n' "$CACHE_ALIAS"
		return
	fi

	local run_id instance
	run_id=$(random_id)
	instance="dorf-proof-cache-$run_id"
	[[ "$instance" =~ ^dorf-proof-cache-[0-9a-f]{12}$ ]] || die "generated cache instance name is unsafe"
	instance_exists "$instance" && die "refusing to reuse existing proof instance $instance"

	printf 'HOST ADMIN -> Ubuntu %s -> Docker + Compose + empty Incus project\n' "$BASE_FINGERPRINT"
	ACTIVE_INSTANCE=$instance
	ACTIVE_ROLE=cache-build
	ACTIVE_CACHE_KEY=$CACHE_KEY
	ACTIVE_RUN_ID=$run_id
	trap cleanup_active_instance EXIT
	outer_incus init "images:$BASE_FINGERPRINT" "$instance" --vm \
		-c security.nesting=true \
		-c limits.cpu=4 \
		-c limits.memory=10GiB \
		-c user.dorf.proof.owner="$PROOF_OWNER" \
		-c user.dorf.proof.role=cache-build \
		-c user.dorf.proof.cache_key="$CACHE_KEY" \
		-c user.dorf.proof.run_id="$run_id" \
		-d root,size=60GiB
	outer_incus start "$instance"
	wait_for_guest "$instance"
	outer_incus file push "$DOCKER_HELPER" "$instance/tmp/dorf-proof-docker.sh"
	outer_incus file push "$INCUS_HELPER" "$instance/tmp/dorf-proof-incus.sh"
	outer_incus file push "$GUEST_SCRIPT" "$instance/tmp/compose-vm-guest.sh"
	outer_incus exec "$instance" -- chmod 0700 \
		/tmp/dorf-proof-docker.sh /tmp/dorf-proof-incus.sh /tmp/compose-vm-guest.sh
	outer_incus exec "$instance" -- env \
		DORF_PROOF_KVM_DEVICE=/dev/kvm \
		/tmp/compose-vm-guest.sh cache-prep \
		--user "$GUEST_USER" \
		--docker-helper /tmp/dorf-proof-docker.sh \
		--docker-sha256 "$DOCKER_SHA" \
		--incus-helper /tmp/dorf-proof-incus.sh \
		--incus-sha256 "$INCUS_SHA" \
		--guest-sha256 "$GUEST_SHA"
	outer_incus exec "$instance" -- rm -f \
		/tmp/dorf-proof-docker.sh /tmp/dorf-proof-incus.sh /tmp/compose-vm-guest.sh
	outer_incus exec "$instance" -- sync

	assert_owned_instance "$instance" cache-build "$CACHE_KEY" "$run_id"
	outer_incus stop "$instance" --timeout 90
	assert_owned_instance "$instance" cache-build "$CACHE_KEY" "$run_id"
	image_exists "$CACHE_ALIAS" && die "refusing to replace cache alias $CACHE_ALIAS"
	# Incus publish models image properties as trailing key=value arguments.
	outer_incus publish "$instance" --alias "$CACHE_ALIAS" \
		"dorf.proof.owner=$PROOF_OWNER" \
		"dorf.proof.schema=$HARNESS_SCHEMA" \
		"dorf.proof.cache_key=$CACHE_KEY" \
		"dorf.proof.base_fingerprint=$BASE_FINGERPRINT" \
		"dorf.proof.docker_sha256=$DOCKER_SHA" \
		"dorf.proof.incus_sha256=$INCUS_SHA" \
		"dorf.proof.guest_sha256=$GUEST_SHA"
	assert_cache_image "$CACHE_ALIAS"
	delete_owned_instance "$instance" cache-build "$CACHE_KEY" "$run_id"
	ACTIVE_INSTANCE=
	trap - EXIT
	printf 'HOST ADMIN -> frozen cache ready: %s\n' "$CACHE_ALIAS"
}

canonical_input_file() {
	local path=$1 label=$2
	[[ -f "$path" && ! -L "$path" ]] || die "$label must be one regular non-symlink file"
	realpath -e -- "$path"
}

prepare_evidence_dir() {
	local root=$1 instance=$2
	[[ "$root" = /* && "$root" != / ]] || die "--evidence-root must be one narrow absolute directory"
	if [[ -e "$root" || -L "$root" ]]; then
		[[ -d "$root" && ! -L "$root" ]] || die "evidence root must be one real directory"
	else
		mkdir -p "$root"
		chmod 0700 "$root"
	fi
	[[ "$(stat -c '%u' "$root")" == "$(id -u)" ]] || die "evidence root must be owned by the current operator"
	[[ "$(stat -c '%a' "$root")" == 700 && "$(realpath -e -- "$root")" == "$root" ]] ||
		die "evidence root must be one real protected 0700 directory"
	local directory="$root/$instance"
	[[ ! -e "$directory" && ! -L "$directory" ]] || die "refusing to replace evidence directory $directory"
	install -d -m 0700 "$directory"
	printf '%s\n' "$directory"
}

stage_ephemeral_key() {
	TEMP_ROOT=$(mktemp -d /tmp/dorf-compose-vm.XXXXXXXX)
	chmod 0700 "$TEMP_ROOT"
	install -m 0600 "$OPENAI_KEY_FILE" "$TEMP_ROOT/openai-key"
}

parse_prove_options() {
	APP_ARCHIVE=
	CHECKSUMS=
	SANDBOX_ARCHIVE=
	SANDBOX_MANIFEST=
	IMAGE_REF=
	OPENAI_KEY_FILE=
	EVIDENCE_ROOT="$PROJECT_ROOT/.dorf/evidence/compose-vm"
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
			OPENAI_KEY_FILE=$2
			shift 2
			;;
		--evidence-root) [[ $# -ge 2 ]] || die "--evidence-root needs a value"; EVIDENCE_ROOT=$2; shift 2 ;;
		*) die "prove received unknown argument '$1'" ;;
		esac
	done
	[[ -n "$IMAGE_REF" ]] || die "prove requires --image-ref"
	[[ "$IMAGE_REF" =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]*$ ]] || die "image reference is invalid"
	APP_ARCHIVE=$(canonical_input_file "$APP_ARCHIVE" "--app-archive")
	CHECKSUMS=$(canonical_input_file "$CHECKSUMS" "--checksums")
	SANDBOX_ARCHIVE=$(canonical_input_file "$SANDBOX_ARCHIVE" "--sandbox-archive")
	SANDBOX_MANIFEST=$(canonical_input_file "$SANDBOX_MANIFEST" "--sandbox-manifest")
	OPENAI_KEY_FILE=$(canonical_input_file "$OPENAI_KEY_FILE" "--openai-key-file")
	[[ "$(stat -c '%u' "$OPENAI_KEY_FILE")" == "$(id -u)" && "$(stat -c '%a' "$OPENAI_KEY_FILE")" == 600 ]] ||
		die "--openai-key-file must be an operator-owned mode 0600 file"
	EPHEMERAL_SECRET=$(<"$OPENAI_KEY_FILE")
	[[ -n "$EPHEMERAL_SECRET" && "$EPHEMERAL_SECRET" != *$'\n'* ]] ||
		die "--openai-key-file must contain one non-empty secret line"
}

prove() {
	parse_prove_options "$@"
	require_command "$INCUS_BIN"
	require_command sha256sum
	require_command realpath
	assert_proof_project
	trap cleanup_active_instance EXIT
	prepare_cache_identity
	image_exists "$CACHE_ALIAS" || die "frozen cache $CACHE_ALIAS is absent; run refresh-cache explicitly"
	assert_cache_image "$CACHE_ALIAS"
	stage_ephemeral_key

	local run_id instance guest_root guest_inbox guest_evidence uid gid evidence_dir
	run_id=$(random_id)
	instance="dorf-proof-run-$run_id"
	[[ "$instance" =~ ^dorf-proof-run-[0-9a-f]{12}$ ]] || die "generated proof instance name is unsafe"
	instance_exists "$instance" && die "refusing to reuse existing proof instance $instance"
	evidence_dir=$(prepare_evidence_dir "$EVIDENCE_ROOT" "$instance")
	guest_root=/run/dorf-compose-vm-proof
	guest_inbox=$guest_root/inbox
	guest_evidence=$guest_root/evidence

	printf 'FROZEN CACHE -> fresh VM -> prepared operator -> public Dorf CLI\n'
	ACTIVE_INSTANCE=$instance
	ACTIVE_ROLE=run
	ACTIVE_CACHE_KEY=$CACHE_KEY
	ACTIVE_RUN_ID=$run_id
	ACTIVE_SECRET_PATH=$guest_root/openai-key
	ACTIVE_EVIDENCE_SOURCE=$guest_evidence
	ACTIVE_EVIDENCE_DIR=$evidence_dir
	outer_incus init "$CACHE_ALIAS" "$instance" --vm \
		-c security.nesting=true \
		-c limits.cpu=4 \
		-c limits.memory=10GiB \
		-c user.dorf.proof.owner="$PROOF_OWNER" \
		-c user.dorf.proof.role=run \
		-c user.dorf.proof.cache_key="$CACHE_KEY" \
		-c user.dorf.proof.run_id="$run_id" \
		-d root,size=80GiB
	outer_incus start "$instance"
	wait_for_guest "$instance"
	uid=$(outer_incus exec "$instance" -- id -u "$GUEST_USER")
	gid=$(outer_incus exec "$instance" -- id -g "$GUEST_USER")
	[[ "$uid" =~ ^[0-9]+$ && "$uid" -ne 0 && "$gid" =~ ^[0-9]+$ ]] || die "cached guest operator identity is invalid"
	outer_incus exec "$instance" -- install -d -o "$uid" -g "$gid" -m 0700 \
		"$guest_root" "$guest_inbox" "$guest_evidence"
	outer_incus file push --uid "$uid" --gid "$gid" --mode 0700 \
		"$GUEST_SCRIPT" "$instance$guest_root/compose-vm-guest.sh"
	local source
	for source in "$APP_ARCHIVE" "$CHECKSUMS" "$SANDBOX_ARCHIVE" "$SANDBOX_MANIFEST"; do
		outer_incus file push --uid "$uid" --gid "$gid" --mode 0600 \
			"$source" "$instance$guest_inbox/$(basename -- "$source")"
	done
	outer_incus file push --uid "$uid" --gid "$gid" --mode 0600 \
		"$TEMP_ROOT/openai-key" "$instance$guest_root/openai-key"

	set +e
	outer_incus exec "$instance" -- runuser -u "$GUEST_USER" -- env -i \
		HOME="/home/$GUEST_USER" USER="$GUEST_USER" \
		PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		DORF_PROOF_KVM_DEVICE=/dev/kvm \
		"$guest_root/compose-vm-guest.sh" prove \
		--app-archive "$guest_inbox/$(basename -- "$APP_ARCHIVE")" \
		--checksums "$guest_inbox/$(basename -- "$CHECKSUMS")" \
		--image-ref "$IMAGE_REF" \
		--sandbox-archive "$guest_inbox/$(basename -- "$SANDBOX_ARCHIVE")" \
		--sandbox-manifest "$guest_inbox/$(basename -- "$SANDBOX_MANIFEST")" \
		--openai-key-file "$guest_root/openai-key" \
		--work-root "$guest_root/work" \
		--evidence-dir "$guest_evidence"
	local proof_status=$?
	set -e

	assert_owned_instance "$instance" run "$CACHE_KEY" "$run_id"
	outer_incus exec "$instance" -- rm -f "$guest_root/openai-key"
	ACTIVE_SECRET_PATH=
	pull_active_evidence
	if [[ "$proof_status" -ne 0 ]]; then
		delete_owned_instance "$instance" run "$CACHE_KEY" "$run_id"
		ACTIVE_INSTANCE=
		cleanup_temp_root
		trap - EXIT
		die "operator proof failed; bounded evidence retained at $evidence_dir"
	fi
	delete_owned_instance "$instance" run "$CACHE_KEY" "$run_id"
	ACTIVE_INSTANCE=
	cleanup_temp_root
	trap - EXIT
	EPHEMERAL_SECRET=
	printf 'ONE-COMMAND SETUP -> Compose recovery -> file bytes -> cleanup: proven\n'
	printf 'Evidence: %s\n' "$evidence_dir"
}

main() {
	[[ $# -ge 1 ]] || { usage >&2; exit 2; }
	local operation=$1
	shift
	case "$operation" in
	refresh-cache)
		[[ $# -eq 0 ]] || die "refresh-cache accepts no arguments"
		require_host_virtualization
		refresh_cache
		;;
	prove)
		require_host_virtualization
		prove "$@"
		;;
	-h | --help)
		usage
		;;
	*)
		usage >&2
		die "unknown operation '$operation'"
		;;
	esac
}

main "$@"
