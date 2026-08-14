# Dorf E2B Template

The E2B profile packages Dorf's credential-free Debian 13 Sandbox baseline as an exact E2B template
build. It reuses the same guest provisioning recipe as Incus; the E2B runtime adapter only consumes
the resulting exact template reference.

- Shared guest construction authority: [`scripts/sandbox/provision-dorf-guest.sh`](../../scripts/sandbox/provision-dorf-guest.sh)
- E2B packaging authority: [`scripts/e2b/build-template.ts`](../../scripts/e2b/build-template.ts)
- Pinned release-tooling dependencies: [`scripts/e2b/package.json`](../../scripts/e2b/package.json)
  and [`scripts/e2b/bun.lock`](../../scripts/e2b/bun.lock)
- Runtime lifecycle and execution adapter: [`internal/e2b`](../../internal/e2b)
- Exact profile qualification: [`internal/e2b/profile_live_test.go`](../../internal/e2b/profile_live_test.go)
- Authenticated Codex endpoint qualification: [`internal/codex/e2b_endpoint_live_test.go`](../../internal/codex/e2b_endpoint_live_test.go)
- Provider credential boundary: [`docs/project/provider-gateway.md`](../project/provider-gateway.md)

From a clean source commit with `E2B_API_KEY` available only in the host environment, run:

```bash
scripts/e2b/build-template.sh
```

The command writes the exact build reference and its source/base/recipe identities to the ignored
`dist/e2b-template/profile.json`. The E2B API key is not copied into the template. Mutable template
names and tags are never runtime profile identities.

Qualify the manifest's exact build through Dorf's native adapter, then confirm its owned Sandbox was
removed:

```bash
DORF_E2B_PROFILE_LIVE=1 \
DORF_E2B_PROFILE_MANIFEST=dist/e2b-template/profile.json \
  .dorf/bin/mise exec -- go test ./internal/e2b \
  -run TestLiveCombinedHarnessProfile -count=1 -v
```

After the deployment has made its private Provider Gateway reachable at one exact HTTPS `/v1` URL,
qualify the provider-neutral route and unchanged Codex Harness:

```bash
DORF_E2B_GATEWAY_LIVE=1 \
DORF_E2B_TEMPLATE='<exact-template-reference>' \
DORF_E2B_PROVIDER_GATEWAY_URL='https://<deployment-owned-host>/v1' \
  .dorf/bin/mise exec -- go test ./internal/codex \
  -run TestLiveE2BScopedGatewayCompletesCodexTurn -count=1 -v
```

The test does not start or own the tunnel. It requires the configured Gateway Connection, admits only
the exact HTTPS hostname to E2B egress, revokes the exact route before Sandbox cleanup, and never
prints route or provider credentials. The Gateway authority documents the temporary proof and the
uncommitted durable-domain boundary.
