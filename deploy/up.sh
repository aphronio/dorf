#!/usr/bin/env bash
# Prepare Dorf's Sandboxes on a cluster that runs Agent Substrate:
# build and push the Sandbox image, then create the atespace, worker pool and template.
# REGISTRY is a registry the cluster pulls from (the Substrate kind setup serves localhost:5001).
set -euo pipefail
shopt -s inherit_errexit
cd "$(dirname "$0")/.."
REGISTRY=${REGISTRY:-localhost:5001}

PIENV=apps/agent/node_modules/@earendil-works/pi-env/bin/pi-env-linux-x64
# Builds are not reproducible, so the tag names the inputs and the digest names the build.
TAG=$(cat images/sandbox/Dockerfile images/sandbox/init.mts "$PIENV/pi-env" | sha256sum | cut -c1-12)
REPO=$REGISTRY/dorf-sandbox:$TAG

ate() { kubectl ate "$@" 2> >(grep -v '^Warning: no --token-file' >&2); }
ate get atespace dorf >/dev/null 2>&1 || ate create atespace dorf >/dev/null
# The pool runs the installed Substrate's worker; only deploy/substrate.sh moves it.
generation() { kubectl -n dorf get deployment/sandbox -o jsonpath='{.metadata.generation}' 2>/dev/null || echo 0; }
before=$(generation)
WORKER_IMAGE=$(deploy/substrate.sh worker-image)
applied=$(WORKER_IMAGE=$WORKER_IMAGE envsubst <deploy/pool.yaml | kubectl apply -f -)
if grep -q 'workerpool.*\(configured\|created\)' <<<"$applied"; then
	for _ in $(seq 15); do [ "$(generation)" != "$before" ] && break; sleep 1; done
fi
kubectl -n dorf rollout status deployment/sandbox --timeout=70m >/dev/null
until [ -z "$(kubectl -n dorf get pods -l ate.dev/worker-pool -o jsonpath='{.items[?(@.metadata.deletionTimestamp)].metadata.name}')" ]; do sleep 2; done

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
