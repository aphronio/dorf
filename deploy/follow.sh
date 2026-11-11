#!/usr/bin/env bash
# Keep this install at the newest commit on main that passed CI. `deploy/follow.sh install <allowed_signers>`
# runs it every 30 s from a systemd timer. It only reaches out: git and GitHub's API, nothing listens.
# Unsigned commits deploy only when they change what Threads may change; any other change waits for a later
# commit signed by a key in the allowed_signers file, which lives outside the repository.
# Each outcome is posted as a GitHub deployment, so a Thread can see what is live; it names no host.
set -euo pipefail
shopt -s inherit_errexit

# Threads may change these; everything else (deploy/, the gate, images, dependencies, CI) needs a signature.
SAFE='^(apps/agent/src/|apps/agent/test/|docs/|apps/web/)'
UNSAFE='^apps/web/(package\.json|bun\.lock)$'
SIGNERS=/etc/dorf/allowed_signers
AUTH=
EMPTY_TREE=4b825dc642cb6eb9a060e54bf8d69288fbee4904

log() { echo "$*" >&2; }

api() {
	local path=$1
	shift
	curl -fsS --retry 2 -H 'accept: application/vnd.github+json' ${AUTH:+-H "@$AUTH"} "https://api.github.com/repos/$REPO/$path" "$@"
}

# One deployment per commit judged, then its outcome; without a GitHub token the outcome is only logged.
DEPLOYMENT=
record() {
	local sha=$1 state=$2 description=$3
	log "$sha: $state, $description"
	[ -n "$AUTH" ] || return 0
	if [ -z "$DEPLOYMENT" ]; then
		DEPLOYMENT=$(jq -n --arg ref "$sha" '{ref: $ref, environment: "dorf", auto_merge: false, required_contexts: [], description: ""}' |
			api deployments -X POST --data @- | jq -r .id)
	fi
	jq -n --arg state "$state" --arg d "$description" '{state: $state, description: $d}' |
		api "deployments/$DEPLOYMENT/statuses" -X POST --data @- >/dev/null
}

healthy() {
	local _
	for _ in $(seq 12); do
		kubectl -n dorf-gate exec deploy/dorf-agent -- node -e '
			const h = { "tailscale-user-login": process.env.DORF_OWNER }
			Promise.all(["/", "/threads"].map((p) => fetch(`http://127.0.0.1:${process.env.DORF_PORT ?? 8091}${p}`, { headers: h })))
				.then((rs) => process.exit(rs.every((r) => r.ok) ? 0 : 1), () => process.exit(1))' 2>/dev/null && return 0
		sleep 5
	done
	return 1
}

backup() {
	kubectl -n dorf-gate exec deploy/dorf-agent -- node -e '
		const fs = require("node:fs"), { DatabaseSync } = require("node:sqlite")
		const dir = `${process.env.DORF_DATA}/backups`
		fs.mkdirSync(dir, { recursive: true })
		new DatabaseSync(`${process.env.DORF_DATA}/agent.sqlite`).prepare("VACUUM INTO ?").run(`${dir}/${process.argv[1]}.sqlite`)
		for (const old of fs.readdirSync(dir).sort().slice(0, -10)) fs.rmSync(`${dir}/${old}`)' "$(date -u +%Y%m%dT%H%M%SZ)-$1"
}

run() {
	git checkout -q --detach "$1" && .agents/setup >/dev/null && deploy/up.sh && healthy
}

follow() {
	cd "$(dirname "$0")/.."
	REPO=$(git remote get-url origin | sed -E 's#^(https://github.com/|git@github.com:)##; s#\.git$##')
	local tip deployed signed changed unsafe
	tip=$(git ls-remote origin refs/heads/main | cut -f1)
	deployed=$(git rev-parse refs/dorf/deployed)
	[ "$tip" = "$deployed" ] || [ "$tip" = "$(git rev-parse -q --verify refs/dorf/judged || true)" ] && return 0

	# The token Threads push with, as a header file only root can read, never on a command line.
	local basic
	basic=$(kubectl -n dorf-secrets get secret github -o jsonpath='{.data.basic}' 2>/dev/null | base64 -d || true)
	if [ -n "$basic" ]; then
		AUTH=$(mktemp)
		trap 'rm -f "$AUTH"' EXIT
		printf 'authorization: Basic %s' "$basic" >"$AUTH"
	fi
	local ci
	ci=$(api "actions/runs?head_sha=$tip&event=push&branch=main" |
		jq -r '[.workflow_runs[] | select(.path == ".github/workflows/ci.yml")][0] | "\(.status) \(.conclusion)"')
	case $ci in
	"completed success") ;;
	completed*)
		git update-ref refs/dorf/judged "$tip"
		log "$tip: CI did not pass"
		return 0
		;;
	*) return 0 ;;
	esac

	git fetch -q origin main
	git update-ref refs/dorf/judged "$tip"
	if ! git merge-base --is-ancestor "$deployed" "$tip"; then
		record "$tip" failure "held: main no longer contains the live commit"
		return 0
	fi
	signed=$(git log --topo-order --format='%H %G?' "$tip" | awk '$2 == "G" && !found { print $1; found = 1 }')
	changed=$(git diff --name-only "${signed:-$EMPTY_TREE}" "$tip")
	unsafe=$({ grep -Ev "$SAFE" <<<"$changed" || true; grep -E "$UNSAFE" <<<"$changed" || true; } | grep . || true)
	if [ -n "$unsafe" ]; then
		log "$tip: unsigned changes outside the safe paths:" $unsafe
		record "$tip" failure "held: an unsigned change outside the paths Threads may change"
		return 0
	fi

	record "$tip" in_progress ""
	if ! backup "$deployed"; then
		record "$tip" failure "held: the backup failed"
		return 0
	fi
	if run "$tip"; then
		git update-ref refs/dorf/deployed "$tip"
		record "$tip" success "live"
	elif run "$deployed"; then
		record "$tip" failure "failed, rolled back"
	else
		record "$tip" error "failed, and the rollback failed too"
	fi
}

install() {
	cd "$(dirname "$0")/.."
	local signers=${1:?usage: deploy/follow.sh install <allowed_signers file>}
	command -v bun jq kubectl docker >/dev/null
	mkdir -p "$(dirname "$SIGNERS")"
	cp "$signers" "$SIGNERS"
	git config gpg.ssh.allowedSignersFile "$SIGNERS"
	git update-ref refs/dorf/deployed HEAD
	cat >/etc/systemd/system/dorf-follow.service <<-EOF
		[Unit]
		Description=Deploy the newest commit on main that passed CI
		[Service]
		Type=oneshot
		TimeoutStartSec=2h
		ExecStart=/bin/bash -lc 'exec $PWD/deploy/follow.sh'
	EOF
	cat >/etc/systemd/system/dorf-follow.timer <<-EOF
		[Unit]
		Description=Check main for a new commit every 30 s
		[Timer]
		OnBootSec=1min
		OnUnitInactiveSec=30s
		[Install]
		WantedBy=timers.target
	EOF
	systemctl daemon-reload
	systemctl enable --now dorf-follow.timer
}

# Parsed whole before it runs, since a deploy checks out another version of this file.
main() {
	case ${1:-} in
	install) shift && install "$@" ;;
	"") follow ;;
	*) log "usage: deploy/follow.sh [install <allowed_signers file>]" && exit 2 ;;
	esac
}
main "$@"
exit
