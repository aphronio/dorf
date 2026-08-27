#!/bin/sh
set -eu

# Reviewed administrator recipe; Dorf never runs it. Upstream references:
# https://linuxcontainers.org/incus/docs/main/installing/
# https://linuxcontainers.org/incus/docs/main/tutorial/first_steps/
# https://linuxcontainers.org/incus/docs/main/api-extensions/#instance_port_forward
# https://github.com/zabbly/incus

usage() {
	cat <<'EOF'
Usage: incus.sh --user SAFE_OPERATOR_USER \
  --acknowledge-incus-root-authority --acknowledge-kvm-device-access \
  [--initialize-pristine]

For a non-root operator, incus-admin is root-equivalent and kvm grants
virtualization-device access; the user must re-login after group changes.
A root operator needs no group change or re-login.
Only Ubuntu 24.04 noble amd64 with systemd, apt, and /dev/kvm is supported.
--initialize-pristine runs "incus admin init --minimal" only when the daemon
has zero storage pools and zero managed networks. Complete daemons and remote
API configuration are preserved; partial initialization is refused.
EOF
}

die() { printf 'incus.sh: refusing: %s\n' "$1" >&2; exit 1; }
reviewed_executable() {
	[ -f "$1" ] && [ ! -L "$1" ] && [ -x "$1" ] &&
		[ "$(/usr/bin/stat -c '%u:%g:%a' "$1" 2>/dev/null || true)" = 0:0:755 ]
}

TARGET_USER=
ACK_AUTHORITY=0
ACK_KVM=0
INITIALIZE=0
while [ "$#" -gt 0 ]; do
	case "$1" in
	--user) [ "$#" -ge 2 ] || die "--user needs a value"; TARGET_USER=$2; shift 2 ;;
	--acknowledge-incus-root-authority) ACK_AUTHORITY=1; shift ;;
	--acknowledge-kvm-device-access) ACK_KVM=1; shift ;;
	--initialize-pristine) INITIALIZE=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument '$1'" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this reviewed recipe as root"
[ -n "$TARGET_USER" ] || die "--user SAFE_OPERATOR_USER is required"
[ "$ACK_AUTHORITY" -eq 1 ] || die "--acknowledge-incus-root-authority is required"
[ "$ACK_KVM" -eq 1 ] || die "--acknowledge-kvm-device-access is required"
printf '%s\n' "$TARGET_USER" | grep -Eq '^[a-z_][a-z0-9_-]{0,31}$' || die "unsafe user name"
TARGET_UID=$(id -u "$TARGET_USER" 2>/dev/null || true)
[ -n "$TARGET_UID" ] || die "--user must name an existing operator user"
TARGET_HOME=$(getent passwd "$TARGET_USER" | awk -F: '{print $6}')
[ -d "$TARGET_HOME" ] || die "the named user has no home directory"

. /etc/os-release
[ "${ID:-}" = ubuntu ] && [ "${VERSION_ID:-}" = 24.04 ] && [ "${VERSION_CODENAME:-}" = noble ] ||
	die "only Ubuntu 24.04 noble is supported"
[ "$(dpkg --print-architecture)" = amd64 ] || die "only amd64 is supported"
[ -d /run/systemd/system ] || die "systemd is not running"
[ -c /dev/kvm ] || die "/dev/kvm is required"

local_incus() { env -i HOME=/root PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/incus --force-local "$@"; }
SOURCE=/etc/apt/sources.list.d/zabbly-incus-stable.sources
KEY=/etc/apt/keyrings/zabbly.asc
guided_install() {
	reviewed_executable /usr/bin/incus &&
		[ "$(dpkg-query -W -f='${db:Status-Status}' incus 2>/dev/null || true)" = installed ] &&
		[ -f "$SOURCE" ] && grep -F 'https://pkgs.zabbly.com/incus/stable' "$SOURCE" >/dev/null &&
		[ -f "$KEY" ] &&
		[ "$(gpg --batch --with-colons --show-keys --fingerprint "$KEY" | awk -F: '$1 == "fpr" {print $10; exit}')" = 4EFC590696CB15B87C73A3AD82CC8797C838DCFD ]
}
incus_for_operator() {
	if [ "$TARGET_UID" -eq 0 ]; then
		local_incus "$@"
	else
		runuser -u "$TARGET_USER" -- env -i HOME="$TARGET_HOME" USER="$TARGET_USER" \
			PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/incus --force-local "$@"
	fi
}
has_extension() { local_incus query /1.0 2>/dev/null | grep -F '"instance_port_forward"' >/dev/null; }
project_ready() {
	[ "$(local_incus project get dorf restricted 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf features.images 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf features.networks 2>/dev/null)" = false ] &&
		[ "$(local_incus project get dorf features.profiles 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf features.storage.volumes 2>/dev/null)" = true ] &&
		[ "$(local_incus project get dorf restricted.networks.access 2>/dev/null)" = incusbr0 ] &&
		[ "$(local_incus project get dorf restricted.storage-pools.access 2>/dev/null)" = default ]
}
count_resources() {
	STORAGE_COUNT=$(local_incus query '/1.0/storage-pools?recursion=1' | grep -o '"name"[[:space:]]*:' | wc -l)
	NETWORK_COUNT=$(local_incus query '/1.0/networks?recursion=1&all-projects=true' | grep -o '"managed"[[:space:]]*:[[:space:]]*true' | wc -l)
}
in_group() { id -nG "$TARGET_USER" | tr ' ' '\n' | grep -Fx "$1" >/dev/null; }
operator_access_ready() {
	if [ "$TARGET_UID" -eq 0 ]; then
		local_incus query /1.0 >/dev/null 2>&1
	else
		in_group incus-admin && in_group kvm && incus_for_operator query /1.0 >/dev/null 2>&1
	fi
}

ensure_operator_access() {
	if [ "$TARGET_UID" -eq 0 ]; then
		local_incus query /1.0 >/dev/null || die "the root operator cannot reach the local Incus daemon"
		return 0
	fi
	usermod -aG incus-admin "$TARGET_USER"
	usermod -aG kvm "$TARGET_USER"
	incus_for_operator query /1.0 >/dev/null || die "the named user cannot reach the local Incus daemon"
}

report_ready() {
	if [ "$TARGET_UID" -eq 0 ]; then
		printf 'Incus/QEMU is ready with instance_port_forward for the root operator; no group change or re-login is required.\n'
	else
		printf 'Incus/QEMU is ready with instance_port_forward. Re-login is required.\n'
	fi
	printf 'incus-admin is root-equivalent; remote API configuration was not changed.\n'
}

if guided_install && command -v qemu-system-x86_64 >/dev/null 2>&1 && systemctl is-enabled --quiet incus.service &&
	systemctl is-active --quiet incus.service && has_extension; then
	count_resources
	if [ "$STORAGE_COUNT" -gt 0 ] && [ "$NETWORK_COUNT" -gt 0 ] &&
		local_incus storage show default >/dev/null 2>&1 &&
		local_incus query /1.0/networks/incusbr0 | grep -E '"managed"[[:space:]]*:[[:space:]]*true' >/dev/null &&
		project_ready &&
		operator_access_ready; then
		printf 'Incus/QEMU is already ready; no changes made.\n'
		exit 0
	fi
fi

if command -v incus >/dev/null 2>&1 || [ -e /usr/bin/incus ] || [ -L /usr/bin/incus ] || [ -e /var/lib/incus ]; then
	guided_install || die "existing Incus is partial, foreign, or not the guided Zabbly install"
else
	for PATHNAME in /var/snap/lxd /snap/bin/lxc /var/lib/lxd /etc/apt/keyrings/zabbly.asc /etc/apt/sources.list.d/zabbly-incus-stable.sources; do
		[ ! -e "$PATHNAME" ] && [ ! -L "$PATHNAME" ] || die "existing Incus/LXD state '$PATHNAME' needs manual review"
	done
	[ "$(dpkg-query -W -f='${db:Status-Status}' lxd 2>/dev/null || true)" != installed ] || die "LXD is installed"
	apt-get update
	DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg
	KEY_TMP=$(mktemp /tmp/dorf-zabbly-key.XXXXXXXX)
	trap 'rm -f "$KEY_TMP"' EXIT HUP INT TERM
	curl --proto '=https' --tlsv1.2 -fsSL https://pkgs.zabbly.com/key.asc -o "$KEY_TMP"
	FINGERPRINT=$(gpg --batch --with-colons --show-keys --fingerprint "$KEY_TMP" | awk -F: '$1 == "fpr" {print $10; exit}')
	[ "$FINGERPRINT" = 4EFC590696CB15B87C73A3AD82CC8797C838DCFD ] || die "unexpected Zabbly signing key"
	install -d -m 0755 /etc/apt/keyrings
	install -m 0644 "$KEY_TMP" /etc/apt/keyrings/zabbly.asc
	cat >/etc/apt/sources.list.d/zabbly-incus-stable.sources <<'EOF'
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: noble
Components: main
Architectures: amd64
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
	chmod 0644 /etc/apt/sources.list.d/zabbly-incus-stable.sources
	apt-get update
	DEBIAN_FRONTEND=noninteractive apt-get install -y incus qemu-system-x86
fi

command -v qemu-system-x86_64 >/dev/null 2>&1 || die "QEMU is unavailable"
systemctl enable --now incus.service
has_extension || die "Incus lacks instance_port_forward; see https://linuxcontainers.org/incus/docs/main/api-extensions/#instance_port_forward"
count_resources
if [ "$STORAGE_COUNT" -eq 0 ] && [ "$NETWORK_COUNT" -eq 0 ]; then
	[ "$INITIALIZE" -eq 1 ] || die "zero storage pools and zero managed networks; initialize manually or pass --initialize-pristine"
	local_incus admin init --minimal
	count_resources
elif [ "$STORAGE_COUNT" -eq 0 ] || [ "$NETWORK_COUNT" -eq 0 ]; then
	die "partial initialization: storage and managed networks must both exist"
fi
[ "$STORAGE_COUNT" -gt 0 ] && [ "$NETWORK_COUNT" -gt 0 ] || die "Incus initialization did not complete"
local_incus storage show default >/dev/null 2>&1 || die "the exact 'default' storage pool is required"
local_incus query /1.0/networks/incusbr0 | grep -E '"managed"[[:space:]]*:[[:space:]]*true' >/dev/null || die "the managed network 'incusbr0' is required"

if ! local_incus project show dorf >/dev/null 2>&1; then
	local_incus project create dorf \
		--config restricted=true \
		--config features.images=true \
		--config features.networks=false \
		--config features.profiles=true \
		--config features.storage.volumes=true \
		--config restricted.networks.access=incusbr0 \
		--config restricted.storage-pools.access=default
fi
project_ready || die "existing project 'dorf' has incompatible restricted authority"
ensure_operator_access
report_ready
