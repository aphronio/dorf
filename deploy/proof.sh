#!/usr/bin/env bash
# Run one agent proof against the cluster's real gate and Substrate: deploy/proof.sh substrate
set -euo pipefail
cd "$(dirname "$0")/.."
proof=${1:?name a proof in apps/agent/test, such as substrate}
dir=$(mktemp -d)
kubectl -n dorf-gate create token dorf-agent --audience api.ate-system.svc --duration 24h >"$dir/token"
kubectl -n dorf-gate port-forward svc/dorf-gate 0:80 >"$dir/forward" 2>/dev/null &
forward=$!
trap 'kill $forward; rm -rf "$dir"' EXIT
for _ in $(seq 100); do port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$dir/forward") && [ -n "$port" ] && break; sleep 0.2; done
gate=http://127.0.0.1:${port:?the gate did not forward}
# Proof Sandboxes start "p-"; one proof runs at a time, so any left are a killed run's.
for name in $(kubectl ate get actors -a dorf -o json 2>/dev/null | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log((JSON.parse(s).actors??[]).map(a=>a.metadata.name).filter(n=>n.startsWith("p-")).join(" ")))'); do
	node -e 'fetch(process.argv[1],{method:"DELETE",headers:{authorization:`Bearer ${require("fs").readFileSync(process.argv[2],"utf8").trim()}`}}).then(r=>process.exit(r.ok||r.status===404?0:1))' "$gate/sandboxes/$name" "$dir/token"
done
DORF_GATE=$gate DORF_TOKEN_FILE=$dir/token node "apps/agent/test/$proof.ts"
