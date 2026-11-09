#!/usr/bin/env bash
# Prepare Dorf's Sandboxes on a cluster that runs Agent Substrate:
# build and push the Sandbox image, then create the atespace, worker pool and template.
# REGISTRY is a registry the cluster pulls from (the Substrate kind setup serves localhost:5001).
set -euo pipefail
cd "$(dirname "$0")/.."
REGISTRY=${REGISTRY:-localhost:5001}

PIENV=apps/agent/node_modules/@earendil-works/pi-env/bin/pi-env-linux-x64
# Builds are not reproducible, so the tag names the inputs and the digest names the build.
TAG=$(cat images/sandbox/Dockerfile images/sandbox/init.mts "$PIENV/pi-env" | sha256sum | cut -c1-12)
REPO=$REGISTRY/dorf-sandbox:$TAG

ate() { kubectl ate "$@" 2> >(grep -v '^Warning: no --token-file' >&2); }
ate get atespace dorf >/dev/null 2>&1 || ate create atespace dorf >/dev/null
kubectl apply -f deploy/pool.yaml >/dev/null
kubectl -n dorf wait workerpool/sandbox --for=jsonpath='{.status.readyReplicas}'=2 --timeout=10m >/dev/null

# Templates are immutable, and Sandboxes keep the template they were made from.
current=$({ ate get actor-template sandbox -a dorf -o json | node -pe 'JSON.parse(require("fs").readFileSync(0)).containers[0].image'; } 2>/dev/null || true)
if [ -z "$current" ]; then
	docker build -q -t "$REPO" --build-context pienv="$PIENV" images/sandbox >/dev/null
	docker push -q "$REPO" >/dev/null
	current=$REPO@$(docker inspect --format '{{index .RepoDigests 0}}' "$REPO" | cut -d@ -f2)
	IMAGE=$current envsubst <deploy/template.yaml | ate create actor-template -f - >/dev/null
elif [[ "$current" != "$REPO@"* ]]; then
	echo "template sandbox uses $current, not $REPO; replacing a template is not supported yet" >&2
	exit 1
fi
echo "sandbox template ready: $current"
