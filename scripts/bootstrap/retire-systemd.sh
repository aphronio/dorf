#!/bin/sh
set -eu

# Reviewed one-time administrator migration; Dorf only materializes this file.
# It retires the three fixed systemd units shipped by the superseded Dorf
# deployment. It never deletes unit files or changes any other service.

PATH=/usr/sbin:/usr/bin:/sbin:/bin
LC_ALL=C
export PATH LC_ALL
umask 077

usage() {
	cat <<'EOF'
Usage: retire-systemd.sh --operator-uid UID --operator-gid GID \
  --acknowledge-retire-legacy-dorf-services

Stops and disables only the three intact legacy Dorf systemd services which
preceded the Compose deployment. No unit files are deleted. Rerunning this
helper is safe after a complete or partial prior run.
EOF
}

die() { printf 'retire-systemd.sh: refusing: %s\n' "$1" >&2; exit 1; }

ACK_RETIRE=0
OPERATOR_UID=
OPERATOR_GID=
while [ "$#" -gt 0 ]; do
	case "$1" in
	--operator-uid) [ "$#" -ge 2 ] || die "--operator-uid needs a value"; OPERATOR_UID=$2; shift 2 ;;
	--operator-gid) [ "$#" -ge 2 ] || die "--operator-gid needs a value"; OPERATOR_GID=$2; shift 2 ;;
	--acknowledge-retire-legacy-dorf-services) ACK_RETIRE=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown argument '$1'" ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run this reviewed migration as root"
[ "$ACK_RETIRE" -eq 1 ] || die "--acknowledge-retire-legacy-dorf-services is required"
printf '%s\n' "$OPERATOR_UID" | grep -Eq '^[1-9][0-9]{0,9}$' || die "--operator-uid must be one positive numeric UID"
printf '%s\n' "$OPERATOR_GID" | grep -Eq '^(0|[1-9][0-9]{0,9})$' || die "--operator-gid must be one non-negative numeric GID"
[ -d /run/systemd/system ] || die "systemd is not running"

SYSTEMCTL=/usr/bin/systemctl
[ -f "$SYSTEMCTL" ] && [ ! -L "$SYSTEMCTL" ] && [ -x "$SYSTEMCTL" ] ||
	die "$SYSTEMCTL must be one non-symlink executable regular file"
SYSTEMCTL_METADATA=$(/usr/bin/stat -c '%u:%a' -- "$SYSTEMCTL" 2>/dev/null || true)
SYSTEMCTL_UID=${SYSTEMCTL_METADATA%%:*}
SYSTEMCTL_MODE=${SYSTEMCTL_METADATA#*:}
[ "$SYSTEMCTL_UID" = 0 ] || die "$SYSTEMCTL is not root-owned"
case "$SYSTEMCTL_MODE" in
"" | *[!0-7]*) die "$SYSTEMCTL has an unsafe mode" ;;
esac
[ "$((0$SYSTEMCTL_MODE & 0100))" -ne 0 ] && [ "$((0$SYSTEMCTL_MODE & 0022))" -eq 0 ] ||
	die "$SYSTEMCTL must be protected and owner-executable"

UNIT_DIR=/etc/systemd/system
validate_unit_file() {
	UNIT_PATH=$1
	[ -f "$UNIT_PATH" ] && [ ! -L "$UNIT_PATH" ] ||
		die "$UNIT_PATH is not one regular legacy unit file"
	UNIT_METADATA=$(/usr/bin/stat -c '%u:%a' -- "$UNIT_PATH" 2>/dev/null || true)
	[ "$UNIT_METADATA" = 0:644 ] || die "$UNIT_PATH is not a protected root-owned 0644 unit file"
}

validate_managed_unit() {
	UNIT=$1
	UNIT_PATH=$2
	[ "$(sed -n '1p' "$UNIT_PATH")" = '# Managed by Dorf. Local edits are refused.' ] ||
		die "$UNIT is not an intact legacy Dorf unit"
	[ "$(sed -n '2p' "$UNIT_PATH")" = "# dorf-unit=$UNIT" ] ||
		die "$UNIT has a different legacy unit identity"
	DIGEST_LINE=$(sed -n '3p' "$UNIT_PATH")
	case "$DIGEST_LINE" in
	'# dorf-sha256='*) ;;
	*) die "$UNIT has no legacy ownership digest" ;;
	esac
	WANT_DIGEST=${DIGEST_LINE#\# dorf-sha256=}
	printf '%s\n' "$WANT_DIGEST" | grep -Eq '^[0-9a-f]{64}$' || die "$UNIT has an invalid legacy ownership digest"
	GOT_DIGEST=$(tail -n +4 "$UNIT_PATH" | sha256sum | awk '{print $1}')
	[ "$GOT_DIGEST" = "$WANT_DIGEST" ] || die "$UNIT legacy contents were modified"
	[ "$(grep -Fxc "User=$OPERATOR_UID" "$UNIT_PATH")" -eq 1 ] || die "$UNIT belongs to a different operator UID"
	[ "$(grep -Fxc "Group=$OPERATOR_GID" "$UNIT_PATH")" -eq 1 ] || die "$UNIT belongs to a different operator GID"
}

validate_cloudflared_unit() {
	UNIT_PATH=$1
	[ "$(grep -Fxc "User=$OPERATOR_UID" "$UNIT_PATH")" -eq 1 ] || die "dorf-cloudflared.service belongs to a different operator UID"
	[ "$(grep -Fxc "Group=$OPERATOR_GID" "$UNIT_PATH")" -eq 1 ] || die "dorf-cloudflared.service belongs to a different operator GID"
	[ "$(grep -Ec '^ExecStart="[^"]+" --no-autoupdate --config "[^"]+" tunnel run$' "$UNIT_PATH")" -eq 1 ] ||
		die "dorf-cloudflared.service has a different execution authority"
	[ "$(awk 'END { print NR }' "$UNIT_PATH")" -eq 16 ] ||
		die "dorf-cloudflared.service has a different legacy unit shape"
	[ "$(tail -c 1 "$UNIT_PATH" | od -An -tu1 | tr -d '[:space:]')" = 10 ] ||
		die "dorf-cloudflared.service has a different legacy unit terminator"
	CLOUDFLARED_NORMALIZED=$(sed \
		-e 's/^User=.*$/User=@operator-uid@/' \
		-e 's/^Group=.*$/Group=@operator-gid@/' \
		-e 's|^ExecStart=.*$|ExecStart="@binary@" --no-autoupdate --config "@config@" tunnel run|' \
		"$UNIT_PATH")
	CLOUDFLARED_EXPECTED='[Unit]
Description=Dorf Cloudflare Tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=@operator-uid@
Group=@operator-gid@
NoNewPrivileges=true
ExecStart="@binary@" --no-autoupdate --config "@config@" tunnel run
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target'
	[ "$CLOUDFLARED_NORMALIZED" = "$CLOUDFLARED_EXPECTED" ] ||
		die "dorf-cloudflared.service is not the exact legacy Dorf Tunnel unit"
}

unit_state() {
	STATE_OPERATION=$1
	STATE_UNIT=$2
	if STATE_VALUE=$("$SYSTEMCTL" "$STATE_OPERATION" -- "$STATE_UNIT" 2>/dev/null); then
		:
	else
		:
	fi
	printf '%s' "$STATE_VALUE"
}

for UNIT in dorf-control-api.service dorf-worker.service dorf-cloudflared.service; do
	UNIT_PATH=$UNIT_DIR/$UNIT
	if [ ! -e "$UNIT_PATH" ] && [ ! -L "$UNIT_PATH" ]; then
		ACTIVE_STATE=$(unit_state is-active "$UNIT")
		ENABLED_STATE=$(unit_state is-enabled "$UNIT")
		case "$ACTIVE_STATE" in inactive | failed | unknown | not-found) ;;
		*) die "$UNIT is active without its intact legacy unit file" ;;
		esac
		case "$ENABLED_STATE" in disabled | static | indirect | generated | transient | masked | masked-runtime | not-found) continue ;;
		*) die "$UNIT is enabled without its intact legacy unit file" ;;
		esac
	fi

	validate_unit_file "$UNIT_PATH"
	case "$UNIT" in
	dorf-control-api.service | dorf-worker.service) validate_managed_unit "$UNIT" "$UNIT_PATH" ;;
	dorf-cloudflared.service) validate_cloudflared_unit "$UNIT_PATH" ;;
	esac

	"$SYSTEMCTL" stop -- "$UNIT" >/dev/null 2>&1 || true
	ACTIVE_STATE=$(unit_state is-active "$UNIT")
	case "$ACTIVE_STATE" in inactive | failed | unknown | not-found) ;;
	*) die "$UNIT did not stop (state: $ACTIVE_STATE)" ;;
	esac

	"$SYSTEMCTL" disable -- "$UNIT" >/dev/null 2>&1 || true
	ENABLED_STATE=$(unit_state is-enabled "$UNIT")
	case "$ENABLED_STATE" in
	disabled | static | indirect | generated | transient | masked | masked-runtime | not-found) ;;
	*) die "$UNIT did not become disabled (state: $ENABLED_STATE)" ;;
	esac
done

printf 'Legacy Dorf systemd services are stopped and disabled. No unit files were deleted.\n'
