#!/bin/sh
set -eu

# Reviewed administrator recipe; Dorf never runs it. Upstream references:
# https://docs.docker.com/engine/install/ubuntu/
# https://docs.docker.com/engine/install/linux-postinstall/
# https://docs.docker.com/compose/install/linux/
# https://docs.docker.com/engine/network/packet-filtering-firewalls/

usage() {
	cat <<'EOF'
Usage: docker.sh --user SAFE_OPERATOR_USER \
  --acknowledge-docker-root-authority --acknowledge-firewall-impact

For a non-root operator, the docker group is root-equivalent and the user must
re-login after group membership changes. A root operator needs no group change.
Docker also changes host forwarding and firewall behavior.
Only Ubuntu 24.04 noble amd64 with apt and systemd is supported.
EOF
}

die() { printf 'docker.sh: refusing: %s\n' "$1" >&2; exit 1; }

TARGET_USER=
ACK_AUTHORITY=0
ACK_FIREWALL=0
while [ "$#" -gt 0 ]; do
	case "$1" in
	--user) [ "$#" -ge 2 ] || die "--user needs a value"; TARGET_USER=$2; shift 2 ;;
	--acknowledge-docker-root-authority) ACK_AUTHORITY=1; shift ;;
	--acknowledge-firewall-impact) ACK_FIREWALL=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument '$1'" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this reviewed recipe as root"
[ -n "$TARGET_USER" ] || die "--user SAFE_OPERATOR_USER is required"
[ "$ACK_AUTHORITY" -eq 1 ] || die "--acknowledge-docker-root-authority is required"
[ "$ACK_FIREWALL" -eq 1 ] || die "--acknowledge-firewall-impact is required"
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
[ -z "${DOCKER_HOST:-}${DOCKER_CONTEXT:-}" ] || die "remote Docker authority is not accepted"

DOCKER_EXECUTABLE=
resolve_docker() {
	DOCKER_EXECUTABLE=
	for DOCKER_CANDIDATE in /usr/bin/docker /usr/local/bin/docker; do
		if [ ! -e "$DOCKER_CANDIDATE" ] && [ ! -L "$DOCKER_CANDIDATE" ]; then
			continue
		fi
		[ -f "$DOCKER_CANDIDATE" ] && [ ! -L "$DOCKER_CANDIDATE" ] && [ -x "$DOCKER_CANDIDATE" ] ||
			die "Docker executable '$DOCKER_CANDIDATE' is not a non-symlink executable regular file"
		DOCKER_METADATA=$(/usr/bin/stat -c '%u:%a' -- "$DOCKER_CANDIDATE" 2>/dev/null || true)
		DOCKER_UID=${DOCKER_METADATA%%:*}
		DOCKER_MODE=${DOCKER_METADATA#*:}
		[ "$DOCKER_UID" = 0 ] || die "Docker executable '$DOCKER_CANDIDATE' is not root-owned"
		case "$DOCKER_MODE" in
		"" | *[!0-7]*) die "Docker executable '$DOCKER_CANDIDATE' has an unsafe mode" ;;
		esac
		[ "$((0$DOCKER_MODE & 0100))" -ne 0 ] && [ "$((0$DOCKER_MODE & 0022))" -eq 0 ] ||
			die "Docker executable '$DOCKER_CANDIDATE' must be protected and root-owned"
		[ -z "$DOCKER_EXECUTABLE" ] ||
			die "Docker executable authority is ambiguous between '$DOCKER_EXECUTABLE' and '$DOCKER_CANDIDATE'"
		DOCKER_EXECUTABLE=$DOCKER_CANDIDATE
	done
}

AMBIENT_DOCKER=$(command -v docker 2>/dev/null || true)
case "$AMBIENT_DOCKER" in
"" | /usr/bin/docker | /usr/local/bin/docker) ;;
*) die "Docker executable '$AMBIENT_DOCKER' is outside the accepted system locations" ;;
esac
resolve_docker

docker_for_operator() {
	if [ "$TARGET_UID" -eq 0 ]; then
		env -i HOME="$TARGET_HOME" USER="$TARGET_USER" \
			PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
			"$DOCKER_EXECUTABLE" "$@"
	else
		runuser -u "$TARGET_USER" -- env -i HOME="$TARGET_HOME" USER="$TARGET_USER" \
			PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
			"$DOCKER_EXECUTABLE" "$@"
	fi
}

docker_ready() {
	[ "$(docker_for_operator context show 2>/dev/null || true)" = default ] &&
		[ "$(docker_for_operator context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null || true)" = unix:///var/run/docker.sock ] &&
		systemctl is-enabled --quiet docker.service && systemctl is-active --quiet docker.service &&
		docker_for_operator info >/dev/null 2>&1 &&
		docker_for_operator compose version >/dev/null 2>&1
}

docker_installation_ready() {
	if [ "$TARGET_UID" -eq 0 ]; then
		docker_ready
	else
		[ "$(docker_for_operator context show 2>/dev/null || true)" = default ] &&
			[ "$(docker_for_operator context inspect default --format '{{(index .Endpoints "docker").Host}}' 2>/dev/null || true)" = unix:///var/run/docker.sock ] &&
			systemctl is-enabled --quiet docker.service && systemctl is-active --quiet docker.service &&
			"$DOCKER_EXECUTABLE" --host unix:///var/run/docker.sock info >/dev/null 2>&1 &&
			docker_for_operator compose version >/dev/null 2>&1
	fi
}

in_group() { id -nG "$TARGET_USER" | tr ' ' '\n' | grep -Fx "$1" >/dev/null; }

ensure_operator_access() {
	if docker_ready; then
		return 0
	fi
	docker_installation_ready || die "an existing Docker installation is partial, remote, or inaccessible"
	[ "$TARGET_UID" -ne 0 ] || die "the root operator cannot reach the local Docker Engine"
	in_group docker && die "the named docker-group user still cannot reach the local Docker Engine"
	usermod -aG docker "$TARGET_USER"
	docker_ready || die "the named user cannot reach Docker after the acknowledged group change"
}

report_ready() {
	if [ "$TARGET_UID" -eq 0 ]; then
		printf 'Docker Engine and Compose are ready for the root operator; no group change or re-login is required.\n'
	else
		printf 'Docker is ready. The docker group is root-equivalent; re-login is required.\n'
	fi
	printf 'Review Docker firewall/network behavior before publishing ports.\n'
}

if [ -n "$DOCKER_EXECUTABLE" ]; then
	if docker_ready; then
		printf 'Docker Engine and Compose are already ready; no changes made.\n'
		exit 0
	fi
	ensure_operator_access
	report_ready
	exit 0
fi

for PACKAGE in docker.io docker-compose docker-compose-v2 podman-docker containerd runc; do
	[ "$(dpkg-query -W -f='${db:Status-Status}' "$PACKAGE" 2>/dev/null || true)" != installed ] ||
		die "conflicting package '$PACKAGE' is installed"
done
for PATHNAME in /etc/apt/keyrings/docker.asc /etc/apt/sources.list.d/docker.sources /var/lib/docker; do
	[ ! -e "$PATHNAME" ] && [ ! -L "$PATHNAME" ] || die "existing Docker state '$PATHNAME' needs manual review"
done

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg
KEY=$(mktemp /tmp/dorf-docker-key.XXXXXXXX)
trap 'rm -f "$KEY"' EXIT HUP INT TERM
curl --proto '=https' --tlsv1.2 -fsSL https://download.docker.com/linux/ubuntu/gpg -o "$KEY"
FINGERPRINT=$(gpg --batch --with-colons --show-keys --fingerprint "$KEY" | awk -F: '$1 == "fpr" {print $10; exit}')
[ "$FINGERPRINT" = 9DC858229FC7DD38854AE2D88D81803C0EBFCD88 ] || die "unexpected Docker signing key"
install -d -m 0755 /etc/apt/keyrings
install -m 0644 "$KEY" /etc/apt/keyrings/docker.asc
cat >/etc/apt/sources.list.d/docker.sources <<'EOF'
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
Architectures: amd64
Signed-By: /etc/apt/keyrings/docker.asc
EOF
chmod 0644 /etc/apt/sources.list.d/docker.sources

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
systemctl enable --now docker.service
resolve_docker
[ -n "$DOCKER_EXECUTABLE" ] || die "Docker installation did not publish an accepted protected executable"
ensure_operator_access
report_ready
