#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly DRIVER="$SCRIPT_DIR/compose-vm.sh"
readonly GUEST="$SCRIPT_DIR/compose-vm-guest.sh"
readonly TEST_ROOT="$(mktemp -d /tmp/dorf-compose-vm-test.XXXXXXXX)"
readonly SHIM_DIR="$TEST_ROOT/shims"
readonly SHIM_STATE="$TEST_ROOT/shim-state"
readonly EVENTS="$TEST_ROOT/events"
readonly NESTED_STATE="$TEST_ROOT/nested"
readonly CPU_INFO="$TEST_ROOT/cpuinfo"
readonly BASE_FINGERPRINT="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
readonly OUTER_PROJECT="dorf-compose-vm-proof-v1"

mkdir -p "$SHIM_DIR" "$SHIM_STATE"
printf 'Y\n' >"$NESTED_STATE"
printf 'flags : fpu vmx sse\n' >"$CPU_INFO"
: >"$EVENTS"

cat >"$SHIM_DIR/incus" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
state=$DORF_PROOF_SHIM_STATE

[[ "${1:-}" == --force-local ]] || {
	printf 'outer Incus call was not pinned to the local daemon\n' >&2
	exit 65
}
shift
if [[ "${1:-}" == project ]]; then
	case "${2:-}" in
	show)
		[[ "${3:-}" == dorf-compose-vm-proof-v1 && -f "$state/project.exists" ]]
		exit
		;;
	create)
		[[ "${3:-}" == dorf-compose-vm-proof-v1 && ! -e "$state/project.exists" ]]
		touch "$state/project.exists"
		shift 3
		while [[ $# -gt 0 ]]; do
			if [[ $1 == -c ]]; then
				key=${2%%=*}
				value=${2#*=}
				printf '%s\n' "$value" >"$state/project.$key"
				shift 2
			else
				shift
			fi
		done
		exit 0
		;;
	get)
		[[ "${3:-}" == dorf-compose-vm-proof-v1 ]]
		cat "$state/project.${4:?}"
		exit 0
		;;
	*) exit 65 ;;
	esac
fi
[[ "${1:-}" == --project && "${2:-}" == dorf-compose-vm-proof-v1 ]] || {
	printf 'outer Incus call escaped the proof project\n' >&2
	exit 65
}
shift 2

if [[ "$*" == "image info images:ubuntu/24.04/cloud --vm" ]]; then
	printf 'Fingerprint: %s\n' "$DORF_PROOF_BASE_FINGERPRINT"
	exit 0
fi
if [[ "${1:-}" == image && "${2:-}" == info ]]; then
	[[ -f "$state/image.alias" && "$(<"$state/image.alias")" == "${3:-}" ]]
	exit
fi
if [[ "${1:-}" == image && "${2:-}" == get-property ]]; then
	key=${4:-}
	if [[ "${DORF_PROOF_FOREIGN_CACHE:-0}" == 1 && "$key" == dorf.proof.owner ]]; then
		printf 'foreign-owner\n'
		exit 0
	fi
	[[ -f "$state/image.$key" ]] && cat "$state/image.$key"
	exit
fi
if [[ "${1:-}" == init ]]; then
	printf '%s\n' "${3:?}" >"$state/instance.name"
	touch "$state/instance.exists"
	shift 3
	while [[ $# -gt 0 ]]; do
		if [[ $1 == -c ]]; then
			key=${2%%=*}
			value=${2#*=}
			printf '%s\n' "$value" >"$state/config.$key"
			shift 2
		else
			shift
		fi
	done
	exit 0
fi
if [[ "${1:-}" == info ]]; then
	[[ -f "$state/instance.exists" && "$(<"$state/instance.name")" == "${2:-}" ]]
	exit
fi
if [[ "${1:-}" == config && "${2:-}" == get ]]; then
	if [[ "${DORF_PROOF_FOREIGN_INSTANCE_ON_CLEANUP:-0}" == 1 && -f "$state/guest.failed" && "${4:-}" == user.dorf.proof.owner ]]; then
		printf 'foreign-owner\n'
		exit 0
	fi
	cat "$state/config.${4:?}"
	exit 0
fi
if [[ "${1:-}" == exec && "$*" == *" id -u ubuntu" ]]; then
	printf '1000\n'
	exit 0
fi
if [[ "${1:-}" == exec && "$*" == *" id -g ubuntu" ]]; then
	printf '1000\n'
	exit 0
fi
if [[ "${1:-}" == exec && "$*" == *"compose-vm-guest.sh prove"* && "${DORF_PROOF_FAIL_GUEST:-0}" == 1 ]]; then
	touch "$state/guest.failed"
	exit 41
fi
if [[ "${1:-}" == publish ]]; then
	shift 2
	while [[ $# -gt 0 ]]; do
		case "$1" in
		--alias)
			printf '%s\n' "$2" >"$state/image.alias"
			shift 2
			;;
		--property)
			printf 'Incus publish accepts trailing key=value properties, not --property\n' >&2
			exit 64
			;;
		*=*)
			key=${1%%=*}
			value=${1#*=}
			printf '%s\n' "$value" >"$state/image.$key"
			shift
			;;
		*) shift ;;
		esac
	done
	exit 0
fi
if [[ "${1:-}" == file && "${2:-}" == pull ]]; then
	destination=${*: -1}
	mkdir -p "$(dirname -- "$destination")"
	printf 'bounded failure log %s\n' "${DORF_PROOF_TEST_SECRET:-no-secret}" >"$destination"
	exit 0
fi
if [[ "${1:-}" == delete ]]; then
	rm -f -- "$state/instance.exists"
	exit 0
fi
exit 0
EOF
chmod 0755 "$SHIM_DIR/incus"

cleanup() {
	case "$TEST_ROOT" in
	/tmp/dorf-compose-vm-test.*) rm -rf -- "$TEST_ROOT" ;;
	esac
}

make_release_assets() {
	local dir=$1
	local docker_helper=${2:-$SCRIPT_DIR/../bootstrap/docker.sh}
	local incus_helper=${3:-$SCRIPT_DIR/../bootstrap/incus.sh}
	local payload="$dir/payload"
	mkdir -p "$dir" "$payload/bootstrap"
	printf '#!/bin/sh\nprintf "dorf 9.8.7\\n"\n' >"$payload/dorf"
	printf 'license\n' >"$payload/LICENSE"
	cp -- "$docker_helper" "$payload/bootstrap/docker.sh"
	cp -- "$incus_helper" "$payload/bootstrap/incus.sh"
	chmod 0755 "$payload/dorf" "$payload/bootstrap/docker.sh" "$payload/bootstrap/incus.sh"
	tar -C "$payload" -czf "$dir/dorf_9.8.7_linux_x86_64.tar.gz" \
		dorf LICENSE bootstrap/docker.sh bootstrap/incus.sh
	printf 'container image\n' >"$dir/dorf_9.8.7_linux_x86_64_container-image.docker.tar"
	printf 'sandbox image\n' >"$dir/dorf-incus-vm-v5-x86_64.tar.gz"
	printf '{"schema_version":1}\n' >"$dir/dorf-incus-vm-v5-x86_64.json"
	(
		cd -- "$dir"
		sha256sum \
			dorf_9.8.7_linux_x86_64.tar.gz \
			dorf_9.8.7_linux_x86_64_container-image.docker.tar \
			>dorf_9.8.7_checksums.txt
	)
}

write_secret_file() {
	local path=$1 secret=$2
	printf '%s\n' "$secret" >"$path"
	chmod 0600 "$path"
}
trap cleanup EXIT

fail() {
	printf 'compose-vm-test: %s\n' "$1" >&2
	exit 1
}

assert_contains() {
	local file=$1
	local value=$2
	grep -F -- "$value" "$file" >/dev/null || fail "$file does not contain: $value"
}

reset_shim() {
	find "$SHIM_STATE" -mindepth 1 -maxdepth 1 -type f -delete
	: >"$EVENTS"
}

run_driver() {
	env PATH="$SHIM_DIR:$PATH" \
		DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_NESTED_KVM_STATE="$NESTED_STATE" \
		DORF_PROOF_RUN_ID=0123456789ab \
		DORF_PROOF_EVENT_LOG="$EVENTS" \
		DORF_PROOF_SHIM_STATE="$SHIM_STATE" \
		DORF_PROOF_BASE_FINGERPRINT="$BASE_FINGERPRINT" \
		"$DRIVER" "$@"
}

test_public_driver_refuses_unknown_operation_before_incus() {
	local output="$TEST_ROOT/unknown.out"
	if "$DRIVER" destroy-everything >"$output" 2>&1; then
		fail "unknown operation succeeded"
	fi
	assert_contains "$output" "refresh-cache"
	assert_contains "$output" "prove"
}

test_host_without_kvm_is_refused_clearly() {
	local output="$TEST_ROOT/kvm.out"
	local events="$TEST_ROOT/kvm.events"
	: >"$events"
	if PATH="$TEST_ROOT/no-tools:$PATH" \
		DORF_PROOF_KVM_DEVICE="$TEST_ROOT/not-kvm" \
		DORF_PROOF_EVENT_LOG="$events" \
		"$DRIVER" refresh-cache >"$output" 2>&1; then
		fail "refresh-cache accepted a host without /dev/kvm"
	fi
	assert_contains "$output" "nested virtualization"
	[[ ! -s "$events" ]] || fail "Incus was contacted before the KVM refusal"
}

test_refresh_cache_is_keyed_and_owned() {
	reset_shim
	local output="$TEST_ROOT/refresh.out"
	run_driver refresh-cache >"$output" 2>&1

	local docker_sha incus_sha guest_sha cache_key cache_alias
	docker_sha=$(sha256sum "$SCRIPT_DIR/../bootstrap/docker.sh" | awk '{print $1}')
	incus_sha=$(sha256sum "$SCRIPT_DIR/../bootstrap/incus.sh" | awk '{print $1}')
	guest_sha=$(sha256sum "$GUEST" | awk '{print $1}')
	cache_key=$(printf 'schema=1\nbase=%s\ndocker=%s\nincus=%s\nguest=%s\n' \
		"$BASE_FINGERPRINT" "$docker_sha" "$incus_sha" "$guest_sha" | sha256sum | awk '{print $1}')
	cache_alias="dorf-proof-cache-v1-${cache_key:0:20}"
	assert_contains "$EVENTS" "init images:$BASE_FINGERPRINT dorf-proof-cache-0123456789ab --vm"
	assert_contains "$EVENTS" "security.nesting=true"
	assert_contains "$EVENTS" "user.dorf.proof.owner=dorf-compose-vm-proof-v1"
	assert_contains "$EVENTS" "user.dorf.proof.role=cache-build"
	assert_contains "$EVENTS" "file push"
	assert_contains "$EVENTS" "compose-vm-guest.sh cache-prep"
	assert_contains "$EVENTS" "dorf.proof.docker_sha256=$docker_sha"
	assert_contains "$EVENTS" "dorf.proof.incus_sha256=$incus_sha"
	assert_contains "$EVENTS" "dorf.proof.guest_sha256=$guest_sha"
	assert_contains "$EVENTS" "publish dorf-proof-cache-0123456789ab --alias $cache_alias"
	assert_contains "$EVENTS" "delete dorf-proof-cache-0123456789ab --force"
	if grep -F -- '--reuse' "$EVENTS" >/dev/null; then
		fail "refresh-cache may not replace an existing alias"
	fi
	assert_contains "$output" "HOST ADMIN"
	assert_contains "$output" "frozen cache ready"
}

test_outer_incus_is_pinned_to_local_proof_project() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	local event
	while IFS= read -r event; do
		case "$event" in
		"--force-local project show $OUTER_PROJECT" | \
			"--force-local project create $OUTER_PROJECT "* | \
			"--force-local project get $OUTER_PROJECT "* | \
			"--force-local --project $OUTER_PROJECT "*) ;;
		*) fail "outer Incus escaped the exact local proof project: $event" ;;
		esac
	done <"$EVENTS"
}

test_refresh_cache_refuses_foreign_proof_project() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	printf 'foreign-owner\n' >"$SHIM_STATE/project.user.dorf.proof.owner"
	: >"$EVENTS"
	local output="$TEST_ROOT/foreign-project.out"
	if run_driver refresh-cache >"$output" 2>&1; then
		fail "refresh-cache accepted a foreign dedicated project"
	fi
	assert_contains "$output" "ownership or project shape differs"
	if grep -F -- "--project $OUTER_PROJECT" "$EVENTS" >/dev/null; then
		fail "refresh-cache touched resources in a foreign project"
	fi
}

test_refresh_cache_refuses_foreign_alias_without_mutation() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	: >"$EVENTS"
	local output="$TEST_ROOT/foreign-cache.out"
	if DORF_PROOF_FOREIGN_CACHE=1 run_driver refresh-cache >"$output" 2>&1; then
		fail "refresh-cache accepted a foreign cache alias"
	fi
	assert_contains "$output" "refusing cache alias"
	if grep -E '(^| )(publish|delete|stop)( |$)' "$EVENTS" >/dev/null; then
		fail "refresh-cache mutated a foreign alias"
	fi
}

test_failed_proof_keeps_bounded_evidence_but_removes_secret_and_vm() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	: >"$EVENTS"
	local assets="$TEST_ROOT/assets"
	local evidence="$TEST_ROOT/evidence"
	local output="$TEST_ROOT/prove-failure.out"
	local secret='sk-proof-never-print-this'
	local secret_file="$TEST_ROOT/prove-failure-openai-key"
	make_release_assets "$assets"
	write_secret_file "$secret_file" "$secret"
	if DORF_PROOF_TEST_SECRET="$secret" \
		DORF_PROOF_FAIL_GUEST=1 \
		run_driver prove \
			--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
			--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
			--checksums "$assets/dorf_9.8.7_checksums.txt" \
			--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
			--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" \
			--openai-key-file "$secret_file" \
			--evidence-root "$evidence" >"$output" 2>&1; then
		fail "prove succeeded after the guest proof failed"
	fi
	assert_contains "$EVENTS" "user.dorf.proof.role=run"
	assert_contains "$EVENTS" "file push"
	assert_contains "$EVENTS" "openai-key"
	assert_contains "$EVENTS" "compose-vm-guest.sh prove"
	assert_contains "$EVENTS" "rm -f /run/dorf-compose-vm-proof/openai-key"
	assert_contains "$EVENTS" "file pull"
	assert_contains "$EVENTS" "delete dorf-proof-run-0123456789ab --force"
	if grep -R -F -- "$secret" "$EVENTS" "$output" "$evidence" >/dev/null 2>&1; then
		fail "ephemeral AI key survived in output or retained evidence"
	fi
	find "$evidence" -type f -name failure-worker.log -print -quit | grep -q . || fail "failure evidence was not retained"
	[[ -f "$secret_file" ]] || fail "outer proof deleted the operator's OpenAI key file"
}

test_prelaunch_failure_removes_plaintext_staging() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	: >"$EVENTS"
	local assets="$TEST_ROOT/prelaunch-assets"
	local evidence="$TEST_ROOT/unprotected-evidence"
	local output="$TEST_ROOT/prelaunch.out"
	local secret_file="$TEST_ROOT/prelaunch-openai-key"
	make_release_assets "$assets"
	write_secret_file "$secret_file" 'sk-proof-prelaunch-cleanup'
	mkdir -p "$evidence"
	chmod 0755 "$evidence"
	local before after
	before=$(find /tmp -maxdepth 1 -type d -name 'dorf-compose-vm.*' -printf '%f\n' | sort)
	if run_driver prove \
		--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$secret_file" \
		--evidence-root "$evidence" >"$output" 2>&1; then
		fail "prove accepted an unprotected evidence root"
	fi
	after=$(find /tmp -maxdepth 1 -type d -name 'dorf-compose-vm.*' -printf '%f\n' | sort)
	[[ "$before" == "$after" ]] || fail "prelaunch failure retained a plaintext staging directory"
	if grep -E '(^| )init( |$)' "$EVENTS" >/dev/null; then
		fail "prelaunch validation failure created a VM"
	fi
}

test_foreign_instance_cleanup_refusal_still_removes_local_secret_staging() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	: >"$EVENTS"
	local assets="$TEST_ROOT/foreign-cleanup-assets"
	local evidence="$TEST_ROOT/foreign-cleanup-evidence"
	local output="$TEST_ROOT/foreign-cleanup.out"
	local secret_file="$TEST_ROOT/foreign-cleanup-openai-key"
	make_release_assets "$assets"
	write_secret_file "$secret_file" 'sk-proof-foreign-cleanup'
	local before after
	before=$(find /tmp -maxdepth 1 -type d -name 'dorf-compose-vm.*' -printf '%f\n' | sort)
	if DORF_PROOF_FAIL_GUEST=1 DORF_PROOF_FOREIGN_INSTANCE_ON_CLEANUP=1 run_driver prove \
		--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$secret_file" \
		--evidence-root "$evidence" >"$output" 2>&1; then
		fail "proof accepted a foreign instance during cleanup"
	fi
	after=$(find /tmp -maxdepth 1 -type d -name 'dorf-compose-vm.*' -printf '%f\n' | sort)
	[[ "$before" == "$after" ]] || fail "foreign instance cleanup refusal retained local plaintext staging"
	assert_contains "$output" "could not remove attested disposable instance"
	if grep -F -- "delete dorf-proof-run-0123456789ab" "$EVENTS" >/dev/null; then
		fail "cleanup deleted an instance after ownership changed"
	fi
}

test_successful_outer_proof_removes_disposable_vm() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	: >"$EVENTS"
	local assets="$TEST_ROOT/success-assets"
	local evidence="$TEST_ROOT/success-evidence"
	local output="$TEST_ROOT/success.out"
	local secret_file="$TEST_ROOT/success-openai-key"
	make_release_assets "$assets"
	write_secret_file "$secret_file" 'sk-proof-success-only'
	run_driver prove \
		--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$secret_file" \
		--evidence-root "$evidence" >"$output" 2>&1
	assert_contains "$output" "PUBLIC CLI -> restart custody -> file bytes -> cleanup: proven"
	assert_contains "$EVENTS" "rm -f /run/dorf-compose-vm-proof/openai-key"
	assert_contains "$EVENTS" "delete dorf-proof-run-0123456789ab --force"
	[[ ! -f "$SHIM_STATE/instance.exists" ]] || fail "successful proof retained its disposable VM"
	[[ -f "$secret_file" ]] || fail "successful proof deleted the operator's OpenAI key file"
}

test_outer_proof_rejects_duplicate_secret_flags_before_mutation() {
	reset_shim
	local assets="$TEST_ROOT/duplicate-secret-assets"
	local output="$TEST_ROOT/duplicate-secret.out"
	local first="$TEST_ROOT/duplicate-secret-one" second="$TEST_ROOT/duplicate-secret-two"
	make_release_assets "$assets"
	write_secret_file "$first" 'sk-proof-first'
	write_secret_file "$second" 'sk-proof-second'
	if run_driver prove \
		--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$first" --openai-key-file "$second" >"$output" 2>&1; then
		fail "outer proof accepted duplicate OpenAI key files"
	fi
	assert_contains "$output" "--openai-key-file may be provided exactly once"
	[[ ! -s "$EVENTS" ]] || fail "duplicate secret flags reached Incus"
	[[ -f "$first" && -f "$second" ]] || fail "duplicate secret refusal deleted an operator key file"
}

test_outer_proof_accepts_only_one_protected_key_file() {
	reset_shim
	local assets="$TEST_ROOT/secret-boundary-assets"
	local unprotected="$TEST_ROOT/unprotected-openai-key"
	local output="$TEST_ROOT/secret-boundary.out"
	make_release_assets "$assets"
	write_secret_file "$unprotected" 'sk-proof-unprotected'
	chmod 0644 "$unprotected"
	if OPENAI_API_KEY='sk-proof-env-must-not-be-consumed' run_driver prove \
		--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$unprotected" >"$output" 2>&1; then
		fail "outer proof accepted an unprotected OpenAI key file"
	fi
	assert_contains "$output" "operator-owned mode 0600"
	[[ ! -s "$EVENTS" ]] || fail "unprotected key file reached Incus"
	: >"$output"
	if OPENAI_API_KEY='sk-proof-env-must-not-be-consumed' run_driver prove \
		--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" >"$output" 2>&1; then
		fail "outer proof consumed OPENAI_API_KEY without a protected file"
	fi
	assert_contains "$output" "--openai-key-file"
	local help="$TEST_ROOT/help.out"
	"$DRIVER" --help >"$help"
	if grep -F -- 'OPENAI_API_KEY' "$help" >/dev/null; then
		fail "public proof usage advertises a raw environment secret"
	fi
}

test_proof_cache_is_bound_to_release_embedded_helpers() {
	reset_shim
	run_driver refresh-cache >/dev/null 2>&1
	: >"$EVENTS"
	local assets="$TEST_ROOT/release-helper-mismatch-assets"
	local changed_helper="$TEST_ROOT/changed-docker-helper.sh"
	local secret_file="$TEST_ROOT/release-helper-mismatch-openai-key"
	local evidence="$TEST_ROOT/release-helper-mismatch-evidence"
	local output="$TEST_ROOT/release-helper-mismatch.out"
	cp -- "$SCRIPT_DIR/../bootstrap/docker.sh" "$changed_helper"
	printf '\n# changed release helper\n' >>"$changed_helper"
	make_release_assets "$assets" "$changed_helper" "$SCRIPT_DIR/../bootstrap/incus.sh"
	write_secret_file "$secret_file" 'sk-proof-helper-mismatch'
	if run_driver prove \
		--app-archive "$assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$secret_file" \
		--evidence-root "$evidence" >"$output" 2>&1; then
		fail "proof accepted a cache built with helpers different from the supplied release"
	fi
	assert_contains "$output" "frozen cache"
	assert_contains "$output" "is absent"
	if grep -F -- " init " "$EVENTS" >/dev/null; then
		fail "helper/cache mismatch launched a proof VM"
	fi
}

make_guest_proof_fixture() {
	local root=$1
	local assets="$root/assets"
	local payload="$root/payload"
	local shims="$root/shims"
	mkdir -p "$assets" "$payload/bootstrap" "$shims"
	printf 'license\n' >"$payload/LICENSE"
	printf '#!/bin/sh\nexit 0\n' >"$payload/bootstrap/docker.sh"
	printf '#!/bin/sh\nexit 0\n' >"$payload/bootstrap/incus.sh"
	cat >"$payload/dorf" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'dorf %s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
case "${1:-}" in
version)
	printf 'dorf 9.8.7\n'
	;;
setup)
	;;
profile)
	printf '{}\n'
	;;
provider)
	printf '{}\n'
	;;
service)
	case "${2:-}" in
	status) printf '{"ready":true}\n' ;;
	logs) printf 'bounded service log\n' ;;
	restart)
		if [[ "${3:-}" == worker ]]; then
			touch "$DORF_PROOF_FAKE_STATE/worker-restarted"
		fi
		;;
	esac
	;;
run)
	printf '{"job_id":"job-proof","scheduled":true}\n'
	;;
inspect)
	if [[ -f "$DORF_PROOF_FAKE_STATE/cleaned" ]]; then
		printf '%s\n' '{"job":{"cleanup_state":"complete"},"current":"Cleaned","observed_facts":{"sandboxes":[{"id":"sandbox-proof","name":"default"}],"agent_runs":[{"state":"completed","turn_outcome":"completed"}],"actions":[{"kind":"sandbox-delete","scope":"sandbox-proof","state":"succeeded"}]}}'
	elif [[ ! -f "$DORF_PROOF_FAKE_STATE/worker-restarted" ]]; then
		printf '%s\n' '{"job":{"cleanup_state":""},"current":"Running","observed_facts":{"sandboxes":[{"id":"sandbox-proof","name":"default"}],"agent_runs":[{"id":"run-old","sandbox_id":"sandbox-proof","state":"completed","turn_outcome":"completed","finished_at":"2026-08-26T11:59:59Z"},{"id":"run-proof","sandbox_id":"sandbox-proof","state":"active","started_at":"2026-08-26T12:00:00Z"}],"actions":[]}}'
	else
		printf '%s\n' '{"job":{"cleanup_state":""},"current":"Open and idle","observed_facts":{"sandboxes":[{"id":"sandbox-proof","name":"default"}],"agent_runs":[{"id":"run-old","sandbox_id":"sandbox-proof","state":"completed","turn_outcome":"completed","finished_at":"2026-08-26T11:59:59Z"},{"id":"run-proof","sandbox_id":"sandbox-proof","state":"completed","turn_outcome":"completed","started_at":"2026-08-26T12:00:00Z","finished_at":"2026-08-26T12:00:01Z"}],"actions":[]}}'
	fi
	;;
sandbox)
	output=
	while [[ $# -gt 0 ]]; do
		if [[ $1 == --output ]]; then output=$2; shift 2; else shift; fi
	done
	cp -- "$DORF_PROOF_FAKE_EXPECTED_FILE" "$output"
	chmod 0600 "$output"
	;;
cleanup)
	touch "$DORF_PROOF_FAKE_STATE/cleaned"
	printf '{"job_id":"job-proof","scheduled":true}\n'
	;;
*)
	printf 'unexpected fake Dorf command: %s\n' "$*" >&2
	exit 1
	;;
esac
EOF
	chmod 0755 "$payload/dorf" "$payload/bootstrap/docker.sh" "$payload/bootstrap/incus.sh"
	tar -czf "$assets/dorf_9.8.7_linux_x86_64.tar.gz" -C "$payload" dorf LICENSE bootstrap
	printf 'container image\n' >"$assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar"
	printf 'sandbox image\n' >"$assets/dorf-incus-vm-v5-x86_64.tar.gz"
	printf '{"schema_version":1}\n' >"$assets/dorf-incus-vm-v5-x86_64.json"
	(
		cd -- "$assets"
		sha256sum dorf_9.8.7_linux_x86_64.tar.gz \
			dorf_9.8.7_linux_x86_64_container-image.docker.tar \
			>dorf_9.8.7_checksums.txt
	)
	cat >"$shims/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker %s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
case "${1:-}" in
load) printf 'Loaded image: ghcr.io/aphronio/dorf:9.8.7\n' ;;
image)
	case "${2:-}" in
	inspect) printf 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n' ;;
	ls) ;;
	*) exit 1 ;;
	esac
	;;
ps)
	if [[ "$*" == "ps -aq" ]]; then exit 0; fi
	case "$*" in
	*com.docker.compose.service=worker*)
		if [[ -f "$DORF_PROOF_FAKE_STATE/worker-restarted" && "${DORF_PROOF_STICKY_WORKER:-0}" != 1 ]]; then
			printf 'worker-id-after\n'
		else
			printf 'worker-id-before\n'
		fi
		;;
	*com.docker.compose.service=control-api*) printf 'api-id\n' ;;
	*com.docker.compose.service=control-reader*) printf 'reader-id\n' ;;
	*com.docker.compose.service=postgres*) printf 'postgres-id\n' ;;
	*) printf 'postgres-id\nworker-id\nreader-id\napi-id\ngateway-id\n' ;;
	esac
	;;
inspect)
	environment='[]'
	mounts='[]'
	networks='{"dorf_application":{}}'
	health='{}'
	case "${2:-}" in
	api-id)
		ports='{"8745/tcp":[{"HostIp":"127.0.0.1","HostPort":"8745"}]}'
		environment='["DORF_DATABASE_URL=postgresql://dorf@postgres/dorf","DORF_CONTROL_READER_ORIGIN=http://control-reader-rpc:8756","DORF_CONTROL_READER_TOKEN=token"]'
		mounts='[{"Destination":"/var/lib/dorf/.config"},{"Destination":"/var/lib/dorf/.local/state/dorf"}]'
		networks='{"dorf_database":{},"dorf_reader":{}}'
		;;
	reader-id)
		ports='{}'
		environment='["DORF_DATABASE_URL=postgresql://dorf@postgres/dorf","DORF_CONTROL_READER_TOKEN=token","E2B_API_KEY="]'
		mounts='[{"Destination":"/var/lib/dorf/.config/dorf"},{"Destination":"/var/lib/dorf/.local/share/dorf"}]'
		networks='{"dorf_database":{},"dorf_provider":{},"dorf_reader":{},"dorf_reader-egress":{}}'
		health='{"Health":{"Status":"healthy"}}'
		;;
	postgres-id)
		ports='{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"54329"}]}'
		networks='{"dorf_database":{}}'
		;;
	*) ports='{}' ;;
	esac
	printf '[{"Config":{"Env":%s},"State":%s,"HostConfig":{"NetworkMode":"dorf_default"},"Mounts":%s,"NetworkSettings":{"Ports":%s,"Networks":%s}}]\n' "$environment" "$health" "$mounts" "$ports" "$networks"
	;;
volume) [[ "${2:-}" == ls ]] ;;
*) exit 1 ;;
esac
EOF
	cat >"$shims/incus" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'inner-incus %s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
case "$*" in
*" network get incusbr0 ipv4.address") printf '10.10.10.1/24\n' ;;
*" query /1.0") printf '{}\n' ;;
*" list --format json") printf '[]\n' ;;
*" image list --format json") printf '[]\n' ;;
*) printf '[]\n' ;;
esac
EOF
	cat >"$shims/id" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
-nG) printf 'aphronio docker incus-admin kvm\n' ;;
*) exec /usr/bin/id "$@" ;;
esac
EOF
	chmod 0755 "$shims/docker" "$shims/incus" "$shims/id"
}

test_guest_proof_uses_public_cli_and_proves_restart_file_and_cleanup() {
	local fixture="$TEST_ROOT/guest-proof"
	local events="$fixture/events"
	local state="$fixture/state"
	local work="$fixture/work"
	local evidence="$fixture/evidence"
	local secret_file="$fixture/openai-key"
	make_guest_proof_fixture "$fixture"
	mkdir -p "$state" "$evidence"
	chmod 0700 "$fixture" "$state" "$evidence"
	printf 'sk-proof-guest-only\n' >"$secret_file"
	chmod 0600 "$secret_file"
	: >"$events"

	if ! env PATH="$fixture/shims:/usr/bin:/bin" \
		HOME="$fixture/home" \
		DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_CPUINFO="$CPU_INFO" \
		DORF_PROOF_NONCE=0123456789abcdef0123456789abcdef \
		DORF_PROOF_POLL_SECONDS=0 \
		DORF_PROOF_WAIT_ATTEMPTS=3 \
		DORF_PROOF_EVENT_LOG="$events" \
		DORF_PROOF_FAKE_STATE="$state" \
		DORF_PROOF_FAKE_EXPECTED_FILE="$work/expected-PROOF.txt" \
		"$GUEST" prove \
		--app-archive "$fixture/assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$fixture/assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$fixture/assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$fixture/assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$fixture/assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$secret_file" \
		--work-root "$work" \
		--evidence-dir "$evidence" >"$fixture/output" 2>&1; then
		sed -n '1,200p' "$fixture/output" >&2
		fail "ordinary-user guest proof failed"
	fi

	assert_contains "$events" "dorf setup --yes --local-image ghcr.io/aphronio/dorf:9.8.7 --sandbox-provider incus --profile compose-vm-proof --connection-auth openai"
	assert_contains "$events" "--openai-api-key-file $secret_file --incus-manifest"
	assert_contains "$events" "--incus-archive $fixture/assets/dorf-incus-vm-v5-x86_64.tar.gz"
	if grep -E '^dorf (profile install|provider connect|service reconcile)' "$events" >/dev/null; then
		fail "guest proof bypassed the guided setup authority"
	fi
	assert_contains "$events" "dorf service status --output json"
	assert_contains "$events" "dorf provider status --profile compose-vm-proof --ai-connection openai-api --json"
	assert_contains "$events" "dorf service restart worker"
	assert_contains "$events" "dorf service restart api"
	assert_contains "$events" "--ai-connection openai-api"
	assert_contains "$events" "dorf sandbox file get sandbox-proof PROOF.txt"
	assert_contains "$events" "dorf cleanup job-proof"
	assert_contains "$events" "inner-incus --force-local --project dorf list --format json"
	[[ ! -e "$secret_file" ]] || fail "guest proof retained its ephemeral key file"
	cmp "$work/expected-PROOF.txt" "$work/observed-PROOF.txt" || fail "guest proof did not compare exact file bytes"
	assert_contains "$fixture/output" "Open and idle"
	jq -e '.observed_facts.agent_runs | any(.id == "run-proof" and .state == "active")' \
		"$evidence/job-inspect-before-worker-restart.json" >/dev/null ||
		fail "proof did not retain the active pre-restart AgentRun snapshot"
	jq -e '.observed_facts.agent_runs | any(.id == "run-proof" and .state == "completed" and .turn_outcome == "completed")' \
		"$evidence/job-inspect.json" >/dev/null ||
		fail "proof did not retain the later completion of the restarted AgentRun"

	local malformed_key="$fixture/malformed-openai-key"
	printf 'sk-proof-remove-on-parse-failure\n' >"$malformed_key"
	chmod 0600 "$malformed_key"
	if env PATH="$fixture/shims:/usr/bin:/bin" \
		DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_CPUINFO="$CPU_INFO" \
		DORF_PROOF_EVENT_LOG="$events" \
		"$GUEST" prove --openai-key-file "$malformed_key" --unexpected >"$fixture/malformed.out" 2>&1; then
		fail "malformed guest proof options succeeded"
	fi
	[[ ! -e "$malformed_key" ]] || fail "guest parse failure retained its attested ephemeral key"

	local duplicate_one="$fixture/duplicate-openai-key-one"
	local duplicate_two="$fixture/duplicate-openai-key-two"
	write_secret_file "$duplicate_one" 'sk-proof-duplicate-one'
	write_secret_file "$duplicate_two" 'sk-proof-duplicate-two'
	if env PATH="$fixture/shims:/usr/bin:/bin" \
		DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_CPUINFO="$CPU_INFO" \
		DORF_PROOF_EVENT_LOG="$events" \
		"$GUEST" prove \
		--openai-key-file "$duplicate_one" \
		--openai-key-file "$duplicate_two" >"$fixture/duplicate.out" 2>&1; then
		fail "guest proof accepted duplicate OpenAI key files"
	fi
	assert_contains "$fixture/duplicate.out" "--openai-key-file may be provided exactly once"
	[[ -f "$duplicate_one" && -f "$duplicate_two" ]] ||
		fail "ambiguous duplicate secret flags deleted an operator-owned file"
}

test_guest_proof_rejects_worker_restart_without_identity_change() {
	local fixture="$TEST_ROOT/guest-sticky-worker"
	local events="$fixture/events"
	local state="$fixture/state"
	local work="$fixture/work"
	local evidence="$fixture/evidence"
	local secret_file="$fixture/openai-key"
	make_guest_proof_fixture "$fixture"
	mkdir -p "$state" "$evidence"
	chmod 0700 "$fixture" "$state" "$evidence"
	write_secret_file "$secret_file" 'sk-proof-sticky-worker'
	: >"$events"
	if env PATH="$fixture/shims:/usr/bin:/bin" \
		HOME="$fixture/home" \
		DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_CPUINFO="$CPU_INFO" \
		DORF_PROOF_NONCE=1123456789abcdef0123456789abcdef \
		DORF_PROOF_POLL_SECONDS=0 \
		DORF_PROOF_WAIT_ATTEMPTS=3 \
		DORF_PROOF_EVENT_LOG="$events" \
		DORF_PROOF_FAKE_STATE="$state" \
		DORF_PROOF_FAKE_EXPECTED_FILE="$work/expected-PROOF.txt" \
		DORF_PROOF_STICKY_WORKER=1 \
		"$GUEST" prove \
		--app-archive "$fixture/assets/dorf_9.8.7_linux_x86_64.tar.gz" \
		--container-image "$fixture/assets/dorf_9.8.7_linux_x86_64_container-image.docker.tar" \
		--checksums "$fixture/assets/dorf_9.8.7_checksums.txt" \
		--sandbox-archive "$fixture/assets/dorf-incus-vm-v5-x86_64.tar.gz" \
		--sandbox-manifest "$fixture/assets/dorf-incus-vm-v5-x86_64.json" \
		--openai-key-file "$secret_file" \
		--work-root "$work" \
		--evidence-dir "$evidence" >"$fixture/output" 2>&1; then
		fail "guest proof accepted a worker restart with unchanged container identity"
	fi
	assert_contains "$fixture/output" "worker container identity did not change"
	[[ ! -e "$secret_file" ]] || fail "failed restart proof retained its ephemeral key"
}

test_guest_cache_prep_runs_only_reviewed_helpers_and_attests_empty_state() {
	local fixture="$TEST_ROOT/cache-prep"
	local shims="$fixture/shims"
	local user_home="$fixture/ubuntu"
	local events="$fixture/events"
	mkdir -p "$shims" "$user_home"
	: >"$events"
	for helper in docker incus; do
		cat >"$fixture/$helper.sh" <<'EOF'
#!/bin/sh
set -eu
printf 'helper %s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
EOF
		chmod 0700 "$fixture/$helper.sh"
	done
	cat >"$shims/id" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
-u) printf '0\n' ;;
"-u ubuntu") printf '1000\n' ;;
"-nG ubuntu") printf 'ubuntu docker incus-admin kvm\n' ;;
*) exit 1 ;;
esac
EOF
	cat >"$shims/getent" <<EOF
#!/usr/bin/env bash
set -euo pipefail
[[ "\$*" == "passwd ubuntu" ]]
printf 'ubuntu:x:1000:1000:Ubuntu:$user_home:/bin/bash\n'
EOF
	cat >"$shims/runuser" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'runuser %s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
[[ "$*" == *"command -v dorf"* ]] && exit 1
[[ "${DORF_PROOF_DENY_INCUS:-0}" == 1 && "$*" == *"incus --force-local"* ]] && exit 42
exit 0
EOF
	cat >"$shims/incus" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'cache-incus %s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
exit 0
EOF
	chmod 0755 "$shims/id" "$shims/getent" "$shims/runuser" "$shims/incus"
	local docker_sha incus_sha guest_sha
	docker_sha=$(sha256sum "$fixture/docker.sh" | awk '{print $1}')
	incus_sha=$(sha256sum "$fixture/incus.sh" | awk '{print $1}')
	guest_sha=$(sha256sum "$GUEST" | awk '{print $1}')

	env PATH="$shims:/usr/bin:/bin" \
		DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_CPUINFO="$CPU_INFO" \
		DORF_PROOF_EVENT_LOG="$events" \
		"$GUEST" cache-prep \
		--user ubuntu \
		--docker-helper "$fixture/docker.sh" \
		--docker-sha256 "$docker_sha" \
		--incus-helper "$fixture/incus.sh" \
		--incus-sha256 "$incus_sha" \
		--guest-sha256 "$guest_sha" >"$fixture/output" 2>&1

	assert_contains "$events" "--acknowledge-docker-root-authority --acknowledge-firewall-impact"
	assert_contains "$events" "--acknowledge-incus-root-authority --acknowledge-kvm-device-access --initialize-pristine"
	assert_contains "$events" "runuser -u ubuntu -- env -i"
	assert_contains "$events" "incus --force-local --project dorf list --format csv -c n"
	assert_contains "$events" "incus --force-local --project dorf image list --format csv -c f"
	assert_contains "$fixture/output" "empty restricted Incus project"
	if env PATH="$shims:/usr/bin:/bin" \
		DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_CPUINFO="$CPU_INFO" \
		DORF_PROOF_EVENT_LOG="$events" \
		DORF_PROOF_DENY_INCUS=1 \
		"$GUEST" cache-prep \
		--user ubuntu \
		--docker-helper "$fixture/docker.sh" \
		--docker-sha256 "$docker_sha" \
		--incus-helper "$fixture/incus.sh" \
		--incus-sha256 "$incus_sha" \
		--guest-sha256 "$guest_sha" >"$fixture/denied.out" 2>&1; then
		fail "cache prep accepted an ordinary user denied Incus access"
	fi
}

test_public_driver_refuses_unknown_operation_before_incus
test_host_without_kvm_is_refused_clearly
test_refresh_cache_is_keyed_and_owned
test_outer_incus_is_pinned_to_local_proof_project
test_refresh_cache_refuses_foreign_proof_project
test_refresh_cache_refuses_foreign_alias_without_mutation
test_failed_proof_keeps_bounded_evidence_but_removes_secret_and_vm
test_prelaunch_failure_removes_plaintext_staging
test_foreign_instance_cleanup_refusal_still_removes_local_secret_staging
test_successful_outer_proof_removes_disposable_vm
test_outer_proof_rejects_duplicate_secret_flags_before_mutation
test_outer_proof_accepts_only_one_protected_key_file
test_proof_cache_is_bound_to_release_embedded_helpers
test_guest_proof_uses_public_cli_and_proves_restart_file_and_cleanup
test_guest_proof_rejects_worker_restart_without_identity_change
test_guest_cache_prep_runs_only_reviewed_helpers_and_attests_empty_state

printf 'Compose VM proof harness tests passed.\n'
