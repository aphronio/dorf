#!/usr/bin/env bash
# Run one agent proof against the cluster's real gate and Substrate: deploy/proof.sh substrate
set -euo pipefail
cd "$(dirname "$0")/.."
proof=${1:?name a proof in apps/agent/test, such as substrate}
dir=$(mktemp -d)
kubectl -n dorf-gate create token dorf-agent --audience dorf-gate --duration 24h >"$dir/token"
kubectl -n dorf-gate port-forward svc/dorf-gate 0:80 >"$dir/forward" 2>/dev/null &
forward=$!
trap 'kill $forward; rm -rf "$dir"' EXIT
for _ in $(seq 100); do port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$dir/forward") && [ -n "$port" ] && break; sleep 0.2; done
gate=http://127.0.0.1:${port:?the gate did not forward}
# Proof Sandboxes start "p-"; one proof runs at a time, so any left are a killed run's.
for name in $(kubectl ate get actors -a dorf -o json 2>/dev/null | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log((JSON.parse(s).actors??[]).map(a=>a.metadata.name).filter(n=>n.startsWith("p-")).join(" ")))'); do
	node -e 'fetch(process.argv[1],{method:"DELETE",headers:{authorization:`Bearer ${require("fs").readFileSync(process.argv[2],"utf8").trim()}`}}).then(r=>process.exit(r.ok||r.status===404?0:1))' "$gate/sandboxes/$name" "$dir/token"
done
# A proof's repository snapshots start "p-" too.
for name in $(kubectl ate get tags -a dorf -o json 2>/dev/null | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log((JSON.parse(s).tags??[]).map(t=>t.metadata.name).filter(n=>n.startsWith("p-")).join(" ")))'); do
	node -e 'fetch(process.argv[1],{method:"DELETE",headers:{authorization:`Bearer ${require("fs").readFileSync(process.argv[2],"utf8").trim()}`}}).then(r=>process.exit(r.ok||r.status===404?0:1))' "$gate/snapshots/$name" "$dir/token"
done
# The proof is code a Thread may have written. It runs as nobody, in a network namespace whose one reachable address is
# the gate, on a hard-linked copy of the tree without .git: it gets the agent's rights and nothing of this host's.
cp -al . "$dir/tree" && rm -rf "$dir/tree/.git"
mkdir "$dir/home" && chown nobody:nogroup "$dir/home" && chmod 711 "$dir" && chmod 644 "$dir/token"
export PIPE='const net=require("net"),[from,to]=process.argv.slice(1).map(a=>/^\d+$/.test(a)?{port:+a,host:"127.0.0.1"}:{path:a});net.createServer(c=>c.pipe(net.connect(to)).pipe(c)).listen(from.path??from.port,from.host,()=>from.path&&require("fs").chmodSync(from.path,0o666))'
node -e "$PIPE" "$dir/gate.sock" "$port" >/dev/null 2>&1 &
bridge=$!
trap 'kill $forward $bridge; rm -rf "$dir"' EXIT
cat >"$dir/run.sh" <<'RUN'
node -e "$PIPE" 8080 "$DIR/gate.sock" >/dev/null 2>&1 &
trap "kill $!" EXIT
for _ in $(seq 50); do node -e 'require("net").connect(8080,"127.0.0.1").on("connect",()=>process.exit(0)).on("error",()=>process.exit(1))' && break; sleep 0.1; done
cd "$DIR/tree" && node "apps/agent/test/$PROOF.ts"
RUN
for _ in $(seq 50); do [ -S "$dir/gate.sock" ] && break; sleep 0.1; done
unshare --net sh -c 'ip link set lo up && exec "$@"' unshare setpriv --reuid=nobody --regid=nogroup --clear-groups \
	env -i PATH="$PATH" HOME="$dir/home" DIR="$dir" PROOF="$proof" PIPE="$PIPE" DORF_GATE=http://127.0.0.1:8080 DORF_TOKEN_FILE="$dir/token" \
	DORF_PROOF_REPO="${DORF_PROOF_REPO:-}" bash "$dir/run.sh"
