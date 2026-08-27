#!/bin/sh
set -eu

# Reviewed workstation administrator recipe; Dorf never runs it. This helper
# does not join a tailnet, change tailnet grants, proxy Incus, or run over SSH.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

usage() {
	cat <<'EOF'
Usage:
  incus-remote.sh offer --acknowledge-remote-incus-exposure \
    [--tailscale-ip IPV4] [--client-name NAME]
  incus-remote.sh inspect --fingerprint SHA256
  incus-remote.sh revoke --fingerprint SHA256 \
    --acknowledge-client-revocation

The offer command exposes native Incus HTTPS only on the workstation's exact
Tailscale IPv4 address at TCP 8443. It requires the prepared restricted dorf
project and sets a 15-minute offer expiry only when the setting is empty.
Configure tailnet membership and grants separately before issuing an offer.
EOF
}

die() { printf 'incus-remote.sh: refusing: %s\n' "$1" >&2; exit 1; }
local_incus() { env -i HOME=/root PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin incus --force-local "$@"; }

ACTION=${1:-}
[ -n "$ACTION" ] || { usage >&2; exit 2; }
shift
TAILSCALE_IP=
CLIENT_NAME=
FINGERPRINT=
ACK_EXPOSURE=0
ACK_REVOCATION=0
while [ "$#" -gt 0 ]; do
	case "$1" in
	--tailscale-ip) [ "$#" -ge 2 ] || die "--tailscale-ip needs a value"; TAILSCALE_IP=$2; shift 2 ;;
	--client-name) [ "$#" -ge 2 ] || die "--client-name needs a value"; CLIENT_NAME=$2; shift 2 ;;
	--fingerprint) [ "$#" -ge 2 ] || die "--fingerprint needs a value"; FINGERPRINT=$2; shift 2 ;;
	--acknowledge-remote-incus-exposure) ACK_EXPOSURE=1; shift ;;
	--acknowledge-client-revocation) ACK_REVOCATION=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument '$1'" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this reviewed recipe as root on the Incus workstation"
[ "$(command -v incus 2>/dev/null || true)" = /usr/bin/incus ] || die "Incus must be the reviewed /usr/bin/incus installation"

require_fingerprint() {
	printf '%s\n' "$FINGERPRINT" | grep -Eq '^[0-9a-f]{64}$' || die "--fingerprint must be one lowercase SHA-256 value"
}

case "$ACTION" in
inspect)
	[ -z "$TAILSCALE_IP$CLIENT_NAME" ] || die "inspect accepts only --fingerprint"
	[ "$ACK_EXPOSURE" -eq 0 ] && [ "$ACK_REVOCATION" -eq 0 ] || die "inspect does not accept acknowledgement flags"
	require_fingerprint
	TRUST=$(local_incus config trust show "$FINGERPRINT" 2>/dev/null) || die "the exact client fingerprint is not retained"
	TYPE=$(printf '%s\n' "$TRUST" | awk '$1 == "type:" { print $2; exit }')
	RESTRICTED=$(printf '%s\n' "$TRUST" | awk '$1 == "restricted:" { print $2; exit }')
	PROJECTS=$(printf '%s\n' "$TRUST" | awk '
		$1 == "projects:" { in_projects = 1; next }
		in_projects && $1 == "-" { print $2; next }
		in_projects && $1 != "-" { in_projects = 0 }
	')
	[ "$TYPE" = client ] || die "the exact fingerprint is not an Incus client certificate"
	[ "$RESTRICTED" = true ] && [ "$PROJECTS" = dorf ] || die "the exact client fingerprint is not restricted only to project dorf"
	printf '%s\n' "$TRUST"
	;;
revoke)
	[ -z "$TAILSCALE_IP$CLIENT_NAME" ] || die "revoke accepts only --fingerprint and its acknowledgement"
	[ "$ACK_EXPOSURE" -eq 0 ] || die "revoke does not accept the exposure acknowledgement"
	[ "$ACK_REVOCATION" -eq 1 ] || die "--acknowledge-client-revocation is required"
	require_fingerprint
	if local_incus config trust show "$FINGERPRINT" >/dev/null 2>&1; then
		local_incus config trust remove "$FINGERPRINT"
		local_incus config trust show "$FINGERPRINT" >/dev/null 2>&1 && die "the exact client fingerprint remains retained"
		printf 'Revoked Incus client fingerprint %s.\n' "$FINGERPRINT"
	else
		printf 'Incus client fingerprint %s is already absent.\n' "$FINGERPRINT"
	fi
	;;
offer)
	[ -z "$FINGERPRINT" ] || die "offer does not accept --fingerprint"
	[ "$ACK_REVOCATION" -eq 0 ] || die "offer does not accept the revocation acknowledgement"
	[ "$ACK_EXPOSURE" -eq 1 ] || die "--acknowledge-remote-incus-exposure is required"
	[ -n "$CLIENT_NAME" ] || CLIENT_NAME=dorf-controller
	printf '%s\n' "$CLIENT_NAME" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.@-]{0,62}$' || die "unsafe client name"
	[ "$(command -v tailscale 2>/dev/null || true)" = /usr/bin/tailscale ] || die "Tailscale must be available at /usr/bin/tailscale"
	LOCAL_TAILSCALE_IPS=$(/usr/bin/tailscale ip -4) || die "read the workstation Tailscale IPv4 address"
	if [ -z "$TAILSCALE_IP" ]; then
		[ "$(printf '%s\n' "$LOCAL_TAILSCALE_IPS" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] ||
			die "the workstation must have exactly one Tailscale IPv4 address or --tailscale-ip must select one"
		TAILSCALE_IP=$(printf '%s\n' "$LOCAL_TAILSCALE_IPS" | awk 'NF { print; exit }')
	fi
	printf '%s\n' "$TAILSCALE_IP" | awk -F. '
		NF != 4 { exit 1 }
		$1 != 100 || $2 < 64 || $2 > 127 { exit 1 }
		{ for (i = 1; i <= 4; i++) if ($i !~ /^[0-9]+$/ || $i > 255) exit 1 }
	' || die "--tailscale-ip must be one Tailscale IPv4 address"
	printf '%s\n' "$LOCAL_TAILSCALE_IPS" | grep -Fx "$TAILSCALE_IP" >/dev/null || die "the workstation does not own the selected Tailscale IPv4 address"

	VERSION=$(local_incus --version)
	MAJOR=$(printf '%s\n' "$VERSION" | sed -n 's/^\([0-9][0-9]*\)\.\([0-9][0-9]*\).*$/\1/p')
	MINOR=$(printf '%s\n' "$VERSION" | sed -n 's/^\([0-9][0-9]*\)\.\([0-9][0-9]*\).*$/\2/p')
	[ -n "$MAJOR" ] && [ -n "$MINOR" ] || die "Incus version is not parseable"
	[ "$MAJOR" -gt 7 ] || { [ "$MAJOR" -eq 7 ] && [ "$MINOR" -ge 3 ]; } || die "Incus 7.3 or newer is required"
	local_incus query /1.0 | grep -F '"instance_port_forward"' >/dev/null || die "Incus lacks instance_port_forward"
	local_incus storage show default >/dev/null 2>&1 || die "the exact default storage pool is required"
	local_incus query /1.0/networks/incusbr0 | grep -E '"managed"[[:space:]]*:[[:space:]]*true' >/dev/null || die "the managed network incusbr0 is required"
	[ "$(local_incus project get dorf restricted 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf features.images 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf features.networks 2>/dev/null)" = false ] &&
		[ "$(local_incus project get dorf features.profiles 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf features.storage.volumes 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf restricted.networks.access 2>/dev/null)" = incusbr0 ] &&
		[ "$(local_incus project get dorf restricted.storage-pools.access 2>/dev/null)" = default ] ||
		die "project dorf does not have the exact prepared restriction config"

	EXPECTED_LISTENER="$TAILSCALE_IP:8443"
	CURRENT_LISTENER=$(local_incus config get core.https_address 2>/dev/null || true)
	CURRENT_EXPIRY=$(local_incus config get core.remote_token_expiry 2>/dev/null || true)
	if [ -n "$CURRENT_LISTENER" ] && [ "$CURRENT_LISTENER" != "$EXPECTED_LISTENER" ]; then
		die "core.https_address conflicts with the exact Tailscale listener"
	fi
	if [ -n "$CURRENT_EXPIRY" ] && [ "$CURRENT_EXPIRY" != 15m ]; then
		die "core.remote_token_expiry conflicts with the required bounded 15m expiry"
	fi
	[ -n "$CURRENT_LISTENER" ] || local_incus config set core.https_address "$EXPECTED_LISTENER"
	[ -n "$CURRENT_EXPIRY" ] || local_incus config set core.remote_token_expiry 15m
	[ "$(local_incus config get core.https_address)" = "$EXPECTED_LISTENER" ] || die "Incus did not retain the exact Tailscale listener"
	[ "$(local_incus config get core.remote_token_expiry)" = 15m ] || die "Incus did not retain the bounded offer expiry"
	[ -x /usr/bin/curl ] || die "/usr/bin/curl is required for the bounded listener check"
	# This probe proves local route liveness only. Enrollment pins the fetched certificate.
	/usr/bin/curl --noproxy '*' --connect-timeout 3 --max-time 5 --insecure --fail --silent --show-error \
		"https://$TAILSCALE_IP:8443/1.0" >/dev/null || die "the exact Tailscale Incus listener is not reachable"
	local_incus config trust add "$CLIENT_NAME" --restricted --projects dorf
	;;
*) usage >&2; exit 2 ;;
esac
