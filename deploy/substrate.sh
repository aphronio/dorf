#!/usr/bin/env bash
# Install, upgrade or roll back Agent Substrate from its released images, following its upgrade
# runbook with no hand steps. The version comes from deploy/substrate.env; `install` also takes
# one. KIND=1 creates a local kind cluster first. Needs git, go, kubectl and docker.
#   deploy/substrate.sh install [version] | upgrade | rollback | worker-image
# The agent drains Sandboxes off workers as they roll, so keep it running throughout.
set -euo pipefail
shopt -s inherit_errexit
DORF=$(cd "$(dirname "$0")/.." && pwd)
source "$DORF/deploy/substrate.env"
REPO=ghcr.io/agent-substrate/substrate
WORK=${ATE_UPGRADE_DIR:-$HOME/ate-upgrade}
RECORDS=${ATE_RECORD_DIR:-$HOME/.cache/ate-setup}/installs
export ATE_INSTALL_KIND=${KIND:-}

step() { local t=$SECONDS; echo "=== $1"; "${@:2}"; echo "=== $1 done in $((SECONDS - t))s"; }
checkout() { [ -d "$2" ] || git clone -q --depth 1 --branch "$1" https://github.com/agent-substrate/substrate "$2"; }
# Image pulls on a fresh node can outlast ate-setup's default 60 s rollout wait.
setup() { (cd "$1" && go run ./cmd/ate-setup "${@:2}" --rollout-timeout 1h); }
record() { ls -t "$RECORDS"/*.yaml | head -1; }
# The running release, from the API server's image tag; a build without one falls back to the record.
installed() {
	local image
	image=$(kubectl -n ate-system get deploy ate-api-server -o jsonpath='{.spec.template.spec.containers[0].image}')
	image=${image%@*}
	case ${image##*/} in
	*:*) echo "${image##*:}" ;;
	*) sed -n 's/^ *substrateVersion: //p' "$(record)" ;;
	esac
}
worker_image() {
	local digest
	digest=$(docker buildx imagetools inspect "$REPO/ateworker-$2:$1" | awk '/^Digest:/{print $2}')
	[ -n "$digest" ] || { echo "no released worker image $REPO/ateworker-$2:$1" >&2; return 1; }
	echo "$REPO/ateworker-$2:$1@$digest"
}

install() {
	local version=${1:-$SUBSTRATE_VERSION} src=$WORK/install
	rm -rf "$src" && mkdir -p "$WORK" && checkout "$version" "$src"
	[ -z "$ATE_INSTALL_KIND" ] || step kind bash -c "cd $src && hack/create-kind-cluster.sh"
	step ate-system setup "$src" deploy ate-system --image-repo "$REPO" --image-tag "$version" \
		--credential-provider='{"name":"k8s.io"}' \
		--ingress-allowed-spiffe-ids=spiffe://cluster.local/ns/ate-system/sa/atenet-client,spiffe://cluster.local/ns/dorf/sa/dorf-gate
	(cd "$src" && go install ./cmd/kubectl-ate)
}

pools() { kubectl get workerpools -A -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,CLASS:.spec.sandboxClasses[0].name,IMAGE:.spec.workerImage' --no-headers; }

# Moves every pool to its worker image for the version, then waits until no old worker is left.
roll_workers() {
	while read -r ns pool class _; do
		local image
		image=$(worker_image "$1" "$class")
		kubectl -n "$ns" patch workerpool "$pool" --type merge -p "{\"spec\":{\"workerImage\":\"$image\"}}" >/dev/null
		kubectl -n "$ns" wait "deployment/$pool" --for=jsonpath='{.spec.template.spec.containers[0].image}'="$image" --timeout=2m >/dev/null
	done < <(pools)
	while read -r ns pool _; do kubectl -n "$ns" rollout status "deployment/$pool" --timeout=70m >/dev/null; done < <(pools)
	until [ -z "$(kubectl get pods -A -l ate.dev/worker-pool -o jsonpath='{.items[?(@.metadata.deletionTimestamp)].metadata.name}')" ]; do sleep 2; done
}

control_plane() {
	local src=$1 config=$2
	step apiserver setup "$src" deploy apiserver --config "$config"
	kubectl -n ate-system rollout status deploy/ate-api-server >/dev/null
	kubectl -n ate-system rollout status deploy/ate-idp-server >/dev/null
	step controller setup "$src" deploy ate-controller --config "$config"
	step atenet setup "$src" deploy atenet --config "$config"
	kubectl -n ate-system rollout status deploy/atenet-router >/dev/null
	kubectl -n ate-system rollout status deploy/atenet-egress >/dev/null
	step sandboxconfig setup "$src" deploy sandboxconfig --config "$config"
}

upgrade() {
	local old new=$SUBSTRATE_VERSION
	old=$(installed)
	[ "$old" != "$new" ] || { echo "already on $new"; return; }
	echo "upgrading $old -> $new"
	rm -rf "$WORK/old" "$WORK/new" && mkdir -p "$WORK"
	cp "$(record)" "$WORK/old.yaml" && sed "s/^  tag: .*/  tag: $new/" "$WORK/old.yaml" >"$WORK/new.yaml"
	checkout "$old" "$WORK/old" && checkout "$new" "$WORK/new"
	kubectl -n ate-system exec postgres-0 -c postgres -- pg_dumpall -U postgres >"$WORK/db-backup.sql"
	step podcert setup "$WORK/new" deploy podcertificate-controller --config "$WORK/new.yaml"
	step atelet setup "$WORK/new" deploy atelet --config "$WORK/new.yaml"
	kubectl -n ate-system rollout status ds/atelet >/dev/null
	step workers roll_workers "$new"
	control_plane "$WORK/new" "$WORK/new.yaml"
	step ate-system setup "$WORK/new" deploy ate-system --config "$WORK/new.yaml" --setup-csi=none
	(cd "$WORK/new" && go install ./cmd/kubectl-ate)
}

# Undoes the last upgrade, every step in reverse.
rollback() {
	local old
	old=$(sed -n 's/^ *substrateVersion: //p' "$WORK/old.yaml")
	echo "rolling back to $old"
	control_plane "$WORK/old" "$WORK/old.yaml"
	(cd "$WORK/old" && go install ./cmd/kubectl-ate)
	step workers roll_workers "$old"
	step atelet setup "$WORK/old" deploy atelet --config "$WORK/old.yaml"
	kubectl -n ate-system rollout status ds/atelet >/dev/null
	step podcert setup "$WORK/old" deploy podcertificate-controller --config "$WORK/old.yaml"
}

case ${1:-} in
install) install "${2:-}" ;;
upgrade) upgrade ;;
rollback) rollback ;;
worker-image) worker_image "$(installed)" gvisor ;;
*) echo "usage: $0 install [version] | upgrade | rollback | worker-image" >&2; exit 2 ;;
esac
