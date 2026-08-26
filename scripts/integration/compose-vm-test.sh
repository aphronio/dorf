#!/usr/bin/env bash
set -euo pipefail

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly DRIVER="$SCRIPT_DIR/compose-vm.sh"
readonly GUEST="$SCRIPT_DIR/compose-vm-guest.sh"
readonly TEST_ROOT="$(mktemp -d /tmp/dorf-compose-vm-test.XXXXXXXX)"

cleanup() {
	case "$TEST_ROOT" in
	/tmp/dorf-compose-vm-test.*) rm -rf -- "$TEST_ROOT" ;;
	esac
}
trap cleanup EXIT

fail() {
	printf 'compose-vm-test: %s\n' "$1" >&2
	exit 1
}

assert_contains() {
	local file=$1 text=$2
	grep -Fq -- "$text" "$file" || fail "expected '$text' in $file"
}

assert_no_mutation() {
	local event
	while IFS= read -r event; do
		case " $event " in
		*" delete "*|*" init "*|*" publish "*|*" start "*|*" stop "*)
			fail "foreign-resource check attempted mutation: $event"
			;;
		esac
	done <"$1"
}

write_foreign_incus_shim() {
	local destination=$1
	cat >"$destination" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DORF_PROOF_EVENT_LOG"
case "$*" in
"--force-local project show dorf-compose-vm-proof-v1") exit 0 ;;
"--force-local project get dorf-compose-vm-proof-v1 user.dorf.proof.owner")
	printf 'somebody-else\n'
	;;
"--force-local project get dorf-compose-vm-proof-v1 features.images") printf 'true\n' ;;
"--force-local project get dorf-compose-vm-proof-v1 features.profiles") printf 'false\n' ;;
*) exit 90 ;;
esac
EOF
	chmod 0755 "$destination"
}

test_shell_syntax() {
	bash -n "$DRIVER" "$GUEST" "$0"
}

test_public_image_inputs() {
	local output="$TEST_ROOT/help.txt"
	"$DRIVER" --help >"$output"
	assert_contains "$output" "--image-ref"
	if grep -Fq -- "--container-image" "$output"; then
		fail "public proof still accepts a transported Docker archive"
	fi
}

test_guest_uses_setup_generated_compose_selection() {
	assert_contains "$GUEST" 'cd "$COMPOSE_DIR"'
	if grep -Fq -- '--file "$COMPOSE_' "$GUEST" || grep -Fq -- '--env-file' "$GUEST"; then
		fail "guest proof bypasses setup-generated Compose file selection"
	fi
	for evidence in setup-base-handoff.log setup-optional-handoff.log compose-base-up.log compose-final-up.log; do
		assert_contains "$DRIVER" "$evidence"
	done
}

test_invalid_image_ref_fails_before_incus() {
	local nested="$TEST_ROOT/nested" output="$TEST_ROOT/image-ref.txt"
	printf 'Y\n' >"$nested"
	if DORF_PROOF_KVM_DEVICE=/dev/null DORF_PROOF_NESTED_KVM_STATE="$nested" \
		"$DRIVER" prove --image-ref 'not a ref' >"$output" 2>&1; then
		fail "driver accepted an invalid image reference"
	fi
	assert_contains "$output" "image reference is invalid"
}

test_guest_rejects_invalid_image_ref() {
	[[ "$(id -u)" -ne 0 ]] || return 0
	local output="$TEST_ROOT/guest-image-ref.txt"
	if "$GUEST" prove --image-ref 'not a ref' >"$output" 2>&1; then
		fail "guest accepted an invalid image reference"
	fi
	assert_contains "$output" "image reference is invalid"
}

test_foreign_project_is_never_mutated() {
	local shim="$TEST_ROOT/incus" events="$TEST_ROOT/incus-events"
	local output="$TEST_ROOT/foreign.txt" nested="$TEST_ROOT/nested"
	printf 'Y\n' >"$nested"
	write_foreign_incus_shim "$shim"
	if DORF_PROOF_KVM_DEVICE=/dev/null \
		DORF_PROOF_NESTED_KVM_STATE="$nested" \
		DORF_PROOF_EVENT_LOG="$events" \
		INCUS_BIN="$shim" \
		"$DRIVER" refresh-cache >"$output" 2>&1; then
		fail "refresh-cache accepted a foreign proof project"
	fi
	assert_contains "$output" "ownership or project shape differs"
	assert_no_mutation "$events"
}

main() {
	test_shell_syntax
	test_public_image_inputs
	test_guest_uses_setup_generated_compose_selection
	test_invalid_image_ref_fails_before_incus
	test_guest_rejects_invalid_image_ref
	test_foreign_project_is_never_mutated
	printf 'compose-vm-test: lean harness contracts pass\n'
}

main "$@"
