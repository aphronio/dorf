#!/usr/bin/env bash
# Prepare Dorf on a cluster that runs Agent Substrate. REGISTRY: one the cluster pulls from.
set -euo pipefail
shopt -s inherit_errexit
cd "$(dirname "$0")/.."
REGISTRY=${REGISTRY:-localhost:5001}
source deploy/substrate.env

PIENV=apps/agent/node_modules/@earendil-works/pi-env/bin/pi-env-linux-x64
# Builds are not reproducible, so the tag names the inputs and the digest names the build.
TAG=$(cat images/sandbox/{Dockerfile,init.mts,package.json,bun.lock,browser-harness} "$PIENV/pi-env" deploy/template.yaml | sha256sum | cut -c1-12)
REPO=$REGISTRY/dorf-sandbox:$TAG
TEMPLATE=sandbox-$TAG

ate() { kubectl ate "$@" 2> >(grep -v '^Warning: no --token-file' >&2); }
ate get atespace dorf >/dev/null 2>&1 || ate create atespace dorf >/dev/null
kubectl apply -f deploy/egress-fence.yaml >/dev/null
kubectl create namespace dorf-secrets --dry-run=client -o yaml | kubectl apply -f - >/dev/null
policy=$(kubectl -n ate-system get configmap k8s-credential-provider-namespace-policy -o jsonpath='{.data.namespace-policy\.yaml}')
if ! grep -q '^- atespace: dorf$' <<<"$policy"; then
	grep -q '^policies: \[\]$' <<<"$policy" || { echo "grant atespace dorf the namespace dorf-secrets in configmap ate-system/k8s-credential-provider-namespace-policy" >&2; exit 1; }
	kubectl -n ate-system create configmap k8s-credential-provider-namespace-policy --dry-run=client -o yaml \
		--from-literal=namespace-policy.yaml=$'policies:\n- atespace: dorf\n  allowedNamespaces: [dorf-secrets]\n' | kubectl apply -f - >/dev/null
	kubectl -n ate-system rollout restart deployment/k8s-credential-provider >/dev/null
	kubectl -n ate-system rollout status deployment/k8s-credential-provider --timeout=5m >/dev/null
fi
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

# A Sandbox cannot resume once its template is gone, so each build gets a template and only unused ones go.
json() { node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(eval(process.argv[1])(JSON.parse(s))))' "$1"; }
if ! ate get actor-template "$TEMPLATE" -a dorf >/dev/null 2>&1; then
	docker build -q -t "$REPO" --build-context pienv="$PIENV" images/sandbox >/dev/null
	docker push -q "$REPO" >/dev/null
	IMAGE=$REPO@$(docker inspect --format '{{index .RepoDigests 0}}' "$REPO" | cut -d@ -f2) TEMPLATE=$TEMPLATE \
		envsubst <deploy/template.yaml | ate create actor-template -f - >/dev/null
fi
for i in $(seq 300); do
	[ -n "$(ate get actor-template "$TEMPLATE" -a dorf -o json | json 't=>t.status?.goldenSnapshotStatus?.goldenTag?.name??""')" ] && break
	[ "$i" = 300 ] && { echo "template $TEMPLATE has no golden snapshot after 10 minutes" >&2; exit 1; }
	sleep 2
done
used=$(ate get actors -a dorf -o json | json 'a=>(a.actors??[]).map(x=>x.actorTemplate.name).join(" ")')
for old in $(ate get actor-templates -a dorf -o json | json 'a=>(a.actorTemplates??[]).map(x=>x.metadata.name).join(" ")'); do
	[ "$old" = "$TEMPLATE" ] || [[ " $used " == *" $old "* ]] || ate delete actor-template "$old" -a dorf >/dev/null
done
# The local build of a template that is gone goes too; the registry keeps what it holds.
kept=$(ate get actor-templates -a dorf -o json | json 'a=>(a.actorTemplates??[]).map(x=>x.metadata.name).join(" ")')
for tag in $(docker images --format '{{.Tag}}' "$REGISTRY/dorf-sandbox"); do
	[[ " $kept " == *" sandbox-$tag "* ]] || docker image rm "$REGISTRY/dorf-sandbox:$tag" >/dev/null
done
echo "sandbox template ready: $TEMPLATE"

grep -q 'ns/dorf-gate/sa/dorf-gate' <<<"$(kubectl -n ate-system get deploy atenet-router -o jsonpath='{.spec.template.spec.containers[0].args}')" ||
	{ echo "atenet-router does not allow dorf-gate; install Substrate with deploy/substrate.sh" >&2; exit 1; }
# Atespace dorf's Substrate rights belong to the gate alone; the agent's token is only good for the gate.
GATE_SA=user:system:serviceaccount:dorf-gate:dorf-gate
if ! policy=$(ate get access-policy -a dorf -o json 2>/dev/null); then
	json "_=>JSON.stringify({metadata:{name:'default'},bindings:[{role:'editor',members:['$GATE_SA']}]})" <<<'{}' | ate create access-policy -a dorf -f - >/dev/null
elif [ "$(json 'p=>p.bindings.map(b=>b.role+":"+b.members.join(",")).join(" ")' <<<"$policy")" != "editor:$GATE_SA" ]; then
	json "p=>JSON.stringify({metadata:{name:'default',uid:p.metadata.uid,version:p.metadata.version},bindings:[{role:'editor',members:['$GATE_SA']}]})" <<<"$policy" | ate update access-policy -a dorf -f - >/dev/null
fi
# Pods run images by digest, so a push to the registry's tag never changes what runs.
pushed() { docker push -q "$1" >/dev/null && echo "${1%:*}@$(docker inspect --format '{{index .RepoDigests 0}}' "$1" | cut -d@ -f2)"; }
GATE=$REGISTRY/dorf-gate:$( (echo "$SUBSTRATE_VERSION"; cat apps/gate/{Dockerfile,package.json,bun.lock,ateapi.ts,gate.ts}) | sha256sum | cut -c1-12)
docker build -q --build-arg SUBSTRATE_VERSION="$SUBSTRATE_VERSION" -t "$GATE" apps/gate >/dev/null
GATE=$(pushed "$GATE")
# GitHub is on when dorf-secrets/github exists.
DORF_GITHUB=$(kubectl -n dorf-secrets get secret github >/dev/null 2>&1 && echo 1 || true)
GATE_IMAGE=$GATE DORF_GITHUB=$DORF_GITHUB envsubst '$GATE_IMAGE $DORF_GITHUB' <deploy/gate.yaml | kubectl apply -f - >/dev/null
kubectl -n dorf-gate delete secret github --ignore-not-found >/dev/null
kubectl -n dorf-gate rollout status deploy/dorf-gate --timeout=5m >/dev/null
echo "gate ready: service dorf-gate/dorf-gate"

# The broker holds the agent's keys: a copy of the GitHub token, and the Claude plan's token from `claude setup-token`.
kubectl get namespace dorf-broker >/dev/null 2>&1 || kubectl create namespace dorf-broker >/dev/null
if [ -n "$DORF_GITHUB" ]; then
	kubectl -n dorf-secrets get secret github -o json |
		json 's=>JSON.stringify({apiVersion:"v1",kind:"Secret",metadata:{name:"github",namespace:"dorf-broker"},data:{basic:s.data.basic}})' |
		kubectl apply -f - >/dev/null
else
	kubectl -n dorf-broker delete secret github --ignore-not-found >/dev/null
fi
kubectl -n dorf-broker get secret claude >/dev/null 2>&1 ||
	echo "no Claude plan token: run \`claude setup-token\`, then pipe it to \`kubectl -n dorf-broker create secret generic claude --from-file=token=/dev/stdin\`" >&2
BROKER_INPUTS=$( (cat deploy/broker.yaml; kubectl -n dorf-broker get secrets -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.resourceVersion} {end}') | sha256sum | cut -c1-12)
BROKER_INPUTS=$BROKER_INPUTS envsubst '$BROKER_INPUTS' <deploy/broker.yaml | kubectl apply -f - >/dev/null
kubectl -n dorf-broker rollout status deploy/broker --timeout=5m >/dev/null
echo "broker ready: service dorf-broker/broker"

AGENT=$REGISTRY/dorf-agent:$(find apps/agent/{Dockerfile,Dockerfile.dockerignore,package.json,bun.lock,src} apps/web -path apps/web/node_modules -prune -o -path apps/web/.next -prune -o -path apps/web/out -prune -o -type f -print0 | sort -z | xargs -0 cat | sha256sum | cut -c1-12)
docker build -q -f apps/agent/Dockerfile -t "$AGENT" apps >/dev/null
AGENT=$(pushed "$AGENT")
AGENT_IMAGE=$AGENT DORF_MODEL=${DORF_MODEL:-claude-plan/opus} DORF_OWNER=${DORF_OWNER:?set DORF_OWNER to the Tailscale login allowed to use Dorf} DORF_GITHUB=$DORF_GITHUB \
	envsubst '$AGENT_IMAGE $DORF_MODEL $DORF_OWNER $DORF_GITHUB' <deploy/agent.yaml | kubectl apply -f - >/dev/null
kubectl -n dorf-gate rollout status deploy/dorf-agent --timeout=5m >/dev/null
echo "agent ready: deployment dorf-gate/dorf-agent"
