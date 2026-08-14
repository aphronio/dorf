# Dorf E2B Template

The E2B profile packages Dorf's credential-free Debian 13 Sandbox baseline as an exact E2B template
build. It reuses the same guest provisioning recipe as Incus; the E2B runtime adapter only consumes
the resulting exact template reference.

- Shared guest construction authority: [`scripts/sandbox/provision-dorf-guest.sh`](../../scripts/sandbox/provision-dorf-guest.sh)
- E2B packaging authority: [`scripts/e2b/build-template.ts`](../../scripts/e2b/build-template.ts)
- Pinned release-tooling dependencies: [`scripts/e2b/package.json`](../../scripts/e2b/package.json)
  and [`scripts/e2b/bun.lock`](../../scripts/e2b/bun.lock)
- Runtime lifecycle and execution adapter: [`internal/e2b`](../../internal/e2b)
- Provider credential boundary: [`docs/project/provider-gateway.md`](../project/provider-gateway.md)

From a clean source commit with `E2B_API_KEY` available only in the host environment, run:

```bash
scripts/e2b/build-template.sh
```

The command writes the exact build reference and its source/base/recipe identities to the ignored
`dist/e2b-template/profile.json`. The E2B API key is not copied into the template. Mutable template
names and tags are never runtime profile identities.
