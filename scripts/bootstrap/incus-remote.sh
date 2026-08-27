#!/bin/sh
set -eu
set -f

# Reviewed workstation administrator recipe; Dorf never runs it. This helper
# does not join a tailnet, change tailnet grants, proxy Incus, or run over SSH.
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH

PROJECT=dorf-remote
NETWORK=dorfbr0
ACL=dorf-egress
POOL=default
BRIDGE_ADDRESS=10.254.254.1/24
BRIDGE_SUBNET=10.254.254.0/24
REJECT_DESTINATIONS=0.0.0.0/8,10.0.0.0/8,100.64.0.0/10,127.0.0.0/8,169.254.0.0/16,172.16.0.0/12,192.0.0.0/24,192.0.2.0/24,192.88.99.0/24,192.168.0.0/16,198.18.0.0/15,198.51.100.0/24,203.0.113.0/24,224.0.0.0/4,240.0.0.0/4
MODULE_FILE=/etc/modules-load.d/dorf-br_netfilter.conf
UFW_DHCP='ufw allow in on dorfbr0 to any port 67 proto udp'
UFW_DNS_UDP='ufw allow in on dorfbr0 to 10.254.254.1 port 53 proto udp'
UFW_DNS_TCP='ufw allow in on dorfbr0 to 10.254.254.1 port 53 proto tcp'
UFW_DENY_PRIVATE_A='ufw route deny in on dorfbr0 from 10.254.254.0/24 to 10.0.0.0/8'
UFW_DENY_CGNAT='ufw route deny in on dorfbr0 from 10.254.254.0/24 to 100.64.0.0/10'
UFW_DENY_LINK_LOCAL='ufw route deny in on dorfbr0 from 10.254.254.0/24 to 169.254.0.0/16'
UFW_DENY_PRIVATE_B='ufw route deny in on dorfbr0 from 10.254.254.0/24 to 172.16.0.0/12'
UFW_DENY_PRIVATE_C='ufw route deny in on dorfbr0 from 10.254.254.0/24 to 192.168.0.0/16'
UFW_ROUTE_ALLOW='ufw route allow in on dorfbr0 from 10.254.254.0/24'

usage() {
	cat <<'EOF'
Usage:
  incus-remote.sh prepare --acknowledge-kernel-module-impact \
    --acknowledge-firewall-impact
  incus-remote.sh offer --acknowledge-remote-incus-exposure \
    [--tailscale-ip IPV4] [--client-name NAME]
  incus-remote.sh inspect --fingerprint SHA256
  incus-remote.sh revoke --fingerprint SHA256 \
    --acknowledge-client-revocation

The prepare command creates only the fixed dorf-remote project, dorfbr0
network, dorf-egress ACL, br_netfilter module-load declaration, and bounded
UFW rules. It refuses incompatible same-named state and converges compatible
partial state. Loading br_netfilter changes host bridge packet processing;
UFW rules change host input and forwarding policy.

The offer command requires that prepared contract, then exposes native Incus
HTTPS only on the workstation's exact Tailscale IPv4 address at TCP 8443.
It sets a 15-minute offer expiry only when the setting is empty and restricts
the offered client certificate to project dorf-remote. Configure tailnet
membership and grants separately before issuing an offer.
EOF
}

die() { printf 'incus-remote.sh: refusing: %s\n' "$1" >&2; exit 1; }
reviewed_executable() {
	[ -f "$1" ] && [ ! -L "$1" ] && [ -x "$1" ] &&
		[ "$(/usr/bin/stat -c '%u:%g:%a' "$1" 2>/dev/null || true)" = 0:0:755 ]
}
local_incus() { env -i HOME=/root PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/incus --force-local "$@"; }

ACTION=${1:-}
[ -n "$ACTION" ] || { usage >&2; exit 2; }
shift
TAILSCALE_IP=
CLIENT_NAME=
FINGERPRINT=
ACK_KERNEL_MODULE=0
ACK_FIREWALL=0
ACK_EXPOSURE=0
ACK_REVOCATION=0
while [ "$#" -gt 0 ]; do
	case "$1" in
	--tailscale-ip) [ "$#" -ge 2 ] || die "--tailscale-ip needs a value"; TAILSCALE_IP=$2; shift 2 ;;
	--client-name) [ "$#" -ge 2 ] || die "--client-name needs a value"; CLIENT_NAME=$2; shift 2 ;;
	--fingerprint) [ "$#" -ge 2 ] || die "--fingerprint needs a value"; FINGERPRINT=$2; shift 2 ;;
	--acknowledge-kernel-module-impact) ACK_KERNEL_MODULE=1; shift ;;
	--acknowledge-firewall-impact) ACK_FIREWALL=1; shift ;;
	--acknowledge-remote-incus-exposure) ACK_EXPOSURE=1; shift ;;
	--acknowledge-client-revocation) ACK_REVOCATION=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument '$1'" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this reviewed recipe as root on the Incus workstation"
reviewed_executable /usr/bin/incus || die "Incus must be the reviewed /usr/bin/incus installation"

require_fingerprint() {
	printf '%s\n' "$FINGERPRINT" | grep -Eq '^[0-9a-f]{64}$' || die "--fingerprint must be one lowercase SHA-256 value"
}

require_incus_capabilities() {
	VERSION=$(local_incus --version)
	MAJOR=$(printf '%s\n' "$VERSION" | sed -n 's/^\([0-9][0-9]*\)\.\([0-9][0-9]*\).*$/\1/p')
	MINOR=$(printf '%s\n' "$VERSION" | sed -n 's/^\([0-9][0-9]*\)\.\([0-9][0-9]*\).*$/\2/p')
	[ -n "$MAJOR" ] && [ -n "$MINOR" ] || die "Incus version is not parseable"
	[ "$MAJOR" -gt 7 ] || { [ "$MAJOR" -eq 7 ] && [ "$MINOR" -ge 3 ]; } || die "Incus 7.3 or newer is required"
	local_incus query /1.0 | grep -F '"instance_port_forward"' >/dev/null || die "Incus lacks instance_port_forward"
	local_incus storage show "$POOL" >/dev/null 2>&1 || die "the exact default storage pool is required"
}

module_file_ready() {
	[ -f "$MODULE_FILE" ] && [ ! -L "$MODULE_FILE" ] || return 1
	[ "$(/usr/bin/stat -c '%u:%g:%a' "$MODULE_FILE" 2>/dev/null || true)" = 0:0:644 ] || return 1
	[ "$(wc -c <"$MODULE_FILE" | tr -d ' ')" = 13 ] || return 1
	[ "$(sed -n '1p' "$MODULE_FILE")" = br_netfilter ]
}

module_loaded() { grep -Eq '^br_netfilter[[:space:]]' /proc/modules; }

network_exists() { local_incus network show "$NETWORK" >/dev/null 2>&1; }
network_config_keys() {
	local_incus network show "$NETWORK" | awk '
		/^config:$/ { copy = 1; next }
		copy && /^[^ ]/ { exit }
		copy && /^  [^ ]/ { sub(/^  /, ""); sub(/:.*/, ""); print }
	' | LC_ALL=C sort | awk 'NR == 1 { keys = $0; next } { keys = keys "," $0 } END { print keys }'
}

network_ready() {
	network_exists || return 1
	local_incus query "/1.0/networks/$NETWORK" | grep -E '"managed"[[:space:]]*:[[:space:]]*true' >/dev/null || return 1
	local_incus query "/1.0/networks/$NETWORK" | grep -E '"type"[[:space:]]*:[[:space:]]*"bridge"' >/dev/null || return 1
	[ "$(network_config_keys)" = ipv4.address,ipv4.dhcp,ipv4.nat,ipv6.address,security.acls,security.acls.default.egress.action,security.acls.default.ingress.action ] || return 1
	[ "$(local_incus network get "$NETWORK" ipv4.address 2>/dev/null)" = "$BRIDGE_ADDRESS" ] &&
		[ "$(local_incus network get "$NETWORK" ipv4.dhcp 2>/dev/null)" = true ] &&
		[ "$(local_incus network get "$NETWORK" ipv4.nat 2>/dev/null)" = true ] &&
		[ "$(local_incus network get "$NETWORK" ipv6.address 2>/dev/null)" = none ] &&
		[ "$(local_incus network get "$NETWORK" security.acls 2>/dev/null)" = "$ACL" ] &&
		[ "$(local_incus network get "$NETWORK" security.acls.default.ingress.action 2>/dev/null)" = reject ] &&
		[ "$(local_incus network get "$NETWORK" security.acls.default.egress.action 2>/dev/null)" = reject ]
}

acl_exists() { local_incus network acl show "$ACL" >/dev/null 2>&1; }
acl_egress() {
	local_incus network acl show "$ACL" | awk '
		/^egress:/ { copy = 1 }
		/^ingress:/ { print; exit }
		copy { print }
	'
}

acl_has_reject() { acl_egress | grep -F "    destination: $REJECT_DESTINATIONS" >/dev/null; }
acl_has_allow() { acl_egress | grep -F '    destination: 0.0.0.0/0' >/dev/null; }

acl_compatible() {
	acl_exists || return 1
	ACL_EGRESS=$(acl_egress)
	case "$ACL_EGRESS" in
	"egress: []
ingress: []") ;;
	"egress:
  - action: reject
    destination: $REJECT_DESTINATIONS
    state: enabled
ingress: []") ;;
	"egress:
  - action: allow
    destination: 0.0.0.0/0
    state: enabled
ingress: []") ;;
	"egress:
  - action: reject
    destination: $REJECT_DESTINATIONS
    state: enabled
  - action: allow
    destination: 0.0.0.0/0
    state: enabled
ingress: []") ;;
	"egress:
  - action: allow
    destination: 0.0.0.0/0
    state: enabled
  - action: reject
    destination: $REJECT_DESTINATIONS
    state: enabled
ingress: []") ;;
	*) return 1 ;;
	esac
}

acl_ready() { acl_compatible && acl_has_reject && acl_has_allow; }

project_exists() { local_incus project show "$PROJECT" >/dev/null 2>&1; }
project_config_keys() {
	local_incus project show "$PROJECT" | awk '
		/^config:$/ { copy = 1; next }
		copy && /^[^ ]/ { exit }
		copy && /^  [^ ]/ { sub(/^  /, ""); sub(/:.*/, ""); print }
	' | LC_ALL=C sort | awk 'NR == 1 { keys = $0; next } { keys = keys "," $0 } END { print keys }'
}

project_ready() {
	project_exists || return 1
	[ "$(project_config_keys)" = features.images,features.networks,features.profiles,features.storage.buckets,features.storage.volumes,limits.instances,limits.virtual-machines,restricted,restricted.networks.access,restricted.storage-pools.access ] || return 1
	[ "$(local_incus project get "$PROJECT" restricted 2>/dev/null)" = true ] &&
		[ "$(local_incus project get "$PROJECT" features.images 2>/dev/null)" = true ] &&
		[ "$(local_incus project get "$PROJECT" features.networks 2>/dev/null)" = false ] &&
		[ "$(local_incus project get "$PROJECT" features.profiles 2>/dev/null)" = true ] &&
		[ "$(local_incus project get "$PROJECT" features.storage.volumes 2>/dev/null)" = true ] &&
		[ "$(local_incus project get "$PROJECT" features.storage.buckets 2>/dev/null)" = false ] &&
		[ "$(local_incus project get "$PROJECT" restricted.networks.access 2>/dev/null)" = "$NETWORK" ] &&
		[ "$(local_incus project get "$PROJECT" restricted.storage-pools.access 2>/dev/null)" = "$POOL" ] &&
		[ "$(local_incus project get "$PROJECT" limits.instances 2>/dev/null)" = 4 ] &&
		[ "$(local_incus project get "$PROJECT" limits.virtual-machines 2>/dev/null)" = 4 ]
}

ufw_rule_present() { /usr/sbin/ufw show added | grep -Fx "$1" >/dev/null; }
expected_ufw_rule() {
	case "$1" in
	"$UFW_DHCP" | "$UFW_DNS_UDP" | "$UFW_DNS_TCP" | "$UFW_DENY_PRIVATE_A" | "$UFW_DENY_CGNAT" | \
		"$UFW_DENY_LINK_LOCAL" | "$UFW_DENY_PRIVATE_B" | "$UFW_DENY_PRIVATE_C" | "$UFW_ROUTE_ALLOW") return 0 ;;
	*) return 1 ;;
	esac
}

preflight_ufw() {
	[ "$(command -v ufw 2>/dev/null || true)" = /usr/sbin/ufw ] || die "UFW must be the reviewed /usr/sbin/ufw installation"
	/usr/sbin/ufw status | grep -F 'Status: active' >/dev/null || die "UFW must be active before preparing dorfbr0"
	UFW_ADDED=$(/usr/sbin/ufw show added)
	while IFS= read -r UFW_RULE; do
		[ -n "$UFW_RULE" ] || continue
		case "$UFW_RULE" in
		*"$NETWORK"*) expected_ufw_rule "$UFW_RULE" || die "existing UFW rule for dorfbr0 is outside the prepared contract" ;;
		esac
	done <<EOF
$UFW_ADDED
EOF
	for UFW_RULE in "$UFW_DHCP" "$UFW_DNS_UDP" "$UFW_DNS_TCP" "$UFW_DENY_PRIVATE_A" "$UFW_DENY_CGNAT" \
		"$UFW_DENY_LINK_LOCAL" "$UFW_DENY_PRIVATE_B" "$UFW_DENY_PRIVATE_C" "$UFW_ROUTE_ALLOW"; do
		[ "$(printf '%s\n' "$UFW_ADDED" | grep -Fxc "$UFW_RULE" || true)" -le 1 ] ||
			die "existing UFW rule for dorfbr0 is duplicated"
	done
}

ufw_order_ready() {
	for UFW_RULE in "$UFW_DHCP" "$UFW_DNS_UDP" "$UFW_DNS_TCP" "$UFW_DENY_PRIVATE_A" "$UFW_DENY_CGNAT" \
		"$UFW_DENY_LINK_LOCAL" "$UFW_DENY_PRIVATE_B" "$UFW_DENY_PRIVATE_C" "$UFW_ROUTE_ALLOW"; do
		ufw_rule_present "$UFW_RULE" || return 1
	done
	UFW_ADDED=$(/usr/sbin/ufw show added)
	ALLOW_LINE=$(printf '%s\n' "$UFW_ADDED" | grep -nFx "$UFW_ROUTE_ALLOW" | sed -n 's/:.*//p')
	[ -n "$ALLOW_LINE" ] || return 1
	for UFW_RULE in "$UFW_DENY_PRIVATE_A" "$UFW_DENY_CGNAT" "$UFW_DENY_LINK_LOCAL" "$UFW_DENY_PRIVATE_B" "$UFW_DENY_PRIVATE_C"; do
		DENY_LINE=$(printf '%s\n' "$UFW_ADDED" | grep -nFx "$UFW_RULE" | sed -n 's/:.*//p')
		[ -n "$DENY_LINE" ] && [ "$DENY_LINE" -lt "$ALLOW_LINE" ] || return 1
	done
}

run_ufw_rule() {
	UFW_RULE=$1
	set -- $UFW_RULE
	[ "$1" = ufw ] || die "internal UFW rule is malformed"
	shift
	/usr/sbin/ufw "$@"
}

ensure_ufw_rule() { ufw_rule_present "$1" || run_ufw_rule "$1"; }
ensure_ufw() {
	ufw_order_ready && return 0
	if ufw_rule_present "$UFW_ROUTE_ALLOW"; then
		set -- $UFW_ROUTE_ALLOW
		shift
		[ "$1" = route ] || die "internal UFW route rule is malformed"
		shift
		/usr/sbin/ufw --force route delete "$@"
	fi
	ensure_ufw_rule "$UFW_DHCP"
	ensure_ufw_rule "$UFW_DNS_UDP"
	ensure_ufw_rule "$UFW_DNS_TCP"
	ensure_ufw_rule "$UFW_DENY_PRIVATE_A"
	ensure_ufw_rule "$UFW_DENY_CGNAT"
	ensure_ufw_rule "$UFW_DENY_LINK_LOCAL"
	ensure_ufw_rule "$UFW_DENY_PRIVATE_B"
	ensure_ufw_rule "$UFW_DENY_PRIVATE_C"
	ensure_ufw_rule "$UFW_ROUTE_ALLOW"
	ufw_order_ready || die "UFW did not retain the exact ordered dorfbr0 policy"
}

prepared_contract() {
	require_incus_capabilities
	module_file_ready && module_loaded || die "br_netfilter is not prepared and loaded"
	acl_ready || die "ACL dorf-egress does not have the exact prepared egress policy"
	network_ready || die "network dorfbr0 does not have the exact prepared isolation config"
	project_ready || die "project dorf-remote does not have the exact prepared restriction config"
	preflight_ufw
	ufw_order_ready || die "UFW does not have the exact ordered dorfbr0 policy"
}

preflight_prepare() {
	require_incus_capabilities
	[ "$(command -v modprobe 2>/dev/null || true)" = /usr/sbin/modprobe ] || die "modprobe must be the reviewed /usr/sbin/modprobe installation"
	if [ -e "$MODULE_FILE" ] || [ -L "$MODULE_FILE" ]; then
		module_file_ready || die "existing br_netfilter module-load declaration is incompatible"
	fi
	preflight_ufw
	if acl_exists; then acl_compatible || die "existing ACL 'dorf-egress' has incompatible egress policy"; fi
	if network_exists; then network_ready || die "existing network 'dorfbr0' has incompatible isolation config"; fi
	if project_exists; then project_ready || die "existing project 'dorf-remote' has incompatible restricted authority"; fi
}

ensure_module() {
	if ! module_file_ready; then
		MODULE_TMP=$(mktemp /etc/modules-load.d/.dorf-br_netfilter.XXXXXXXX)
		trap 'rm -f "$MODULE_TMP"' 0 1 2 15
		printf 'br_netfilter\n' >"$MODULE_TMP"
		chmod 0644 "$MODULE_TMP"
		chown 0:0 "$MODULE_TMP"
		[ ! -e "$MODULE_FILE" ] && [ ! -L "$MODULE_FILE" ] || die "br_netfilter module-load declaration appeared during preparation"
		mv "$MODULE_TMP" "$MODULE_FILE"
		trap - 0 1 2 15
	fi
	module_loaded || /usr/sbin/modprobe br_netfilter
	module_file_ready && module_loaded || die "br_netfilter was not prepared and loaded"
}

ensure_acl() {
	acl_exists || local_incus network acl create "$ACL"
	acl_has_reject || local_incus network acl rule add "$ACL" egress action=reject destination="$REJECT_DESTINATIONS"
	acl_has_allow || local_incus network acl rule add "$ACL" egress action=allow destination=0.0.0.0/0
	acl_ready || die "Incus did not retain the exact dorf-egress policy"
}

ensure_network() {
	if ! network_exists; then
		local_incus network create "$NETWORK" \
			ipv4.address="$BRIDGE_ADDRESS" ipv4.dhcp=true ipv4.nat=true ipv6.address=none \
			security.acls="$ACL" security.acls.default.ingress.action=reject security.acls.default.egress.action=reject
	fi
	network_ready || die "Incus did not retain the exact dorfbr0 isolation config"
}

ensure_project() {
	if ! project_exists; then
		local_incus project create "$PROJECT" \
			--config restricted=true --config features.images=true --config features.networks=false \
			--config features.profiles=true --config features.storage.volumes=true --config features.storage.buckets=false \
			--config restricted.networks.access=dorfbr0 --config restricted.storage-pools.access=default \
			--config limits.instances=4 --config limits.virtual-machines=4
	fi
	project_ready || die "Incus did not retain the exact dorf-remote restriction config"
}

case "$ACTION" in
prepare)
	[ -z "$TAILSCALE_IP$CLIENT_NAME$FINGERPRINT" ] || die "prepare does not accept client or fingerprint arguments"
	[ "$ACK_EXPOSURE" -eq 0 ] && [ "$ACK_REVOCATION" -eq 0 ] || die "prepare accepts only its kernel-module and firewall acknowledgements"
	[ "$ACK_KERNEL_MODULE" -eq 1 ] || die "--acknowledge-kernel-module-impact is required"
	[ "$ACK_FIREWALL" -eq 1 ] || die "--acknowledge-firewall-impact is required"
	preflight_prepare
	ensure_module
	ensure_acl
	ensure_network
	ensure_project
	ensure_ufw
	prepared_contract
	printf 'Prepared isolated remote Incus project %s on %s; no Tailscale configuration or HTTPS listener was changed.\n' "$PROJECT" "$NETWORK"
	;;
inspect)
	[ -z "$TAILSCALE_IP$CLIENT_NAME" ] || die "inspect accepts only --fingerprint"
	[ "$ACK_KERNEL_MODULE" -eq 0 ] && [ "$ACK_FIREWALL" -eq 0 ] && [ "$ACK_EXPOSURE" -eq 0 ] && [ "$ACK_REVOCATION" -eq 0 ] || die "inspect does not accept acknowledgement flags"
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
	[ "$RESTRICTED" = true ] && [ "$PROJECTS" = dorf-remote ] || die "the exact client fingerprint is not restricted only to project dorf-remote"
	printf '%s\n' "$TRUST"
	;;
revoke)
	[ -z "$TAILSCALE_IP$CLIENT_NAME" ] || die "revoke accepts only --fingerprint and its acknowledgement"
	[ "$ACK_KERNEL_MODULE" -eq 0 ] && [ "$ACK_FIREWALL" -eq 0 ] && [ "$ACK_EXPOSURE" -eq 0 ] || die "revoke does not accept other acknowledgements"
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
	[ "$ACK_KERNEL_MODULE" -eq 0 ] && [ "$ACK_FIREWALL" -eq 0 ] && [ "$ACK_REVOCATION" -eq 0 ] || die "offer does not accept preparation or revocation acknowledgements"
	[ "$ACK_EXPOSURE" -eq 1 ] || die "--acknowledge-remote-incus-exposure is required"
	prepared_contract
	[ -n "$CLIENT_NAME" ] || CLIENT_NAME=dorf-controller
	printf '%s\n' "$CLIENT_NAME" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_.@-]{0,62}$' || die "unsafe client name"
	reviewed_executable /usr/bin/tailscale || die "Tailscale must be the reviewed /usr/bin/tailscale installation"
	LOCAL_TAILSCALE_IPS=$(/usr/bin/tailscale ip -4) || die "read the workstation Tailscale IPv4 address"
	if [ -z "$TAILSCALE_IP" ]; then
		[ "$(printf '%s\n' "$LOCAL_TAILSCALE_IPS" | awk 'NF { count++ } END { print count + 0 }')" -eq 1 ] || die "the workstation must have exactly one Tailscale IPv4 address or --tailscale-ip must select one"
		TAILSCALE_IP=$(printf '%s\n' "$LOCAL_TAILSCALE_IPS" | awk 'NF { print; exit }')
	fi
	printf '%s\n' "$TAILSCALE_IP" | awk -F. '
		NF != 4 { exit 1 }
		$1 != 100 || $2 < 64 || $2 > 127 { exit 1 }
		{ for (i = 1; i <= 4; i++) if ($i !~ /^[0-9]+$/ || $i > 255) exit 1 }
	' || die "--tailscale-ip must be one Tailscale IPv4 address"
	printf '%s\n' "$LOCAL_TAILSCALE_IPS" | grep -Fx "$TAILSCALE_IP" >/dev/null || die "the workstation does not own the selected Tailscale IPv4 address"

	EXPECTED_LISTENER="$TAILSCALE_IP:8443"
	CURRENT_LISTENER=$(local_incus config get core.https_address 2>/dev/null || true)
	CURRENT_EXPIRY=$(local_incus config get core.remote_token_expiry 2>/dev/null || true)
	if [ -n "$CURRENT_LISTENER" ] && [ "$CURRENT_LISTENER" != "$EXPECTED_LISTENER" ]; then die "core.https_address conflicts with the exact Tailscale listener"; fi
	if [ -n "$CURRENT_EXPIRY" ] && [ "$CURRENT_EXPIRY" != 15M ]; then die "core.remote_token_expiry conflicts with the required bounded 15-minute expiry"; fi
	[ -n "$CURRENT_LISTENER" ] || local_incus config set "core.https_address=$EXPECTED_LISTENER"
	[ -n "$CURRENT_EXPIRY" ] || local_incus config set core.remote_token_expiry=15M
	[ "$(local_incus config get core.https_address)" = "$EXPECTED_LISTENER" ] || die "Incus did not retain the exact Tailscale listener"
	[ "$(local_incus config get core.remote_token_expiry)" = 15M ] || die "Incus did not retain the bounded offer expiry"
	[ -x /usr/bin/curl ] || die "/usr/bin/curl is required for the bounded listener check"
	# This probe proves local route liveness only. Enrollment pins the fetched certificate.
	/usr/bin/curl --noproxy '*' --connect-timeout 3 --max-time 5 --insecure --fail --silent --show-error "https://$TAILSCALE_IP:8443/1.0" >/dev/null || die "the exact Tailscale Incus listener is not reachable"
	local_incus config trust add "$CLIENT_NAME" --restricted --projects dorf-remote --quiet
	;;
*) usage >&2; exit 2 ;;
esac
