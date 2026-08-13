# Dorf Incus Image

Dorf's official Sandbox image is a credential-free x86_64 Debian 13 VM used by the Go application's
Incus provider. It supplies a small cross-repository workstation baseline; managed repositories own
their pinned libraries, test tools, services, and setup through `commands.prepare`.

The accepted boundary and rationale are recorded in D064 in
[`docs/project/decisions.md`](../project/decisions.md).

## Image definition

[`scripts/incus/build-dorf-codex-image.sh`](../../scripts/incus/build-dorf-codex-image.sh) is the
single source of truth for the base image, tool versions, integrity pins, installed contents, and
construction procedure. Do not duplicate that inventory here.

Build and publish a local candidate alias with:

```bash
scripts/incus/build-dorf-codex-image.sh
```

`IMAGE_ALIAS` changes the local alias. The builder resolves the official Debian image to an
immutable fingerprint and copies only its versioned provisioning recipe into the fresh VM.

## Candidate proof and publication

[`scripts/incus/release-dorf-codex-image.sh`](../../scripts/incus/release-dorf-codex-image.sh) owns
the candidate build, real no-change AgentRun proof, cleanup, export, manifest creation, and optional
GitHub publication. Run it from a clean source commit already available on GitHub after the normal
repository checks:

```bash
scripts/dev/prepare.sh
scripts/dev/check.sh

PROVIDER_CONNECTION=personal-chatgpt \
GITHUB_INSTALLATION_ID=INSTALLATION_ID \
  scripts/incus/release-dorf-codex-image.sh
```

Pass `--publish` to publish the proven image and Go application in one immutable `vX.Y.Z` release:

```bash
PROVIDER_CONNECTION=personal-chatgpt \
GITHUB_INSTALLATION_ID=INSTALLATION_ID \
  scripts/incus/release-dorf-codex-image.sh --publish
```

The image assets are:

```text
dorf-codex-incus-vm-v4-x86_64.tar.gz
dorf-codex-incus-vm-v4-x86_64.json
```

The proof is intentionally narrower than the complete coding-to-PR terminal. Its exact assertions
and retained evidence shape belong to the release script rather than this guide.
`EVIDENCE_POLICY=retain` keeps local redacted proof under
`dist/room-image/workstation-evidence`; `EVIDENCE_POLICY=remove` deletes it after validation.

## Consumption and credentials

`dorf image install` accepts only an immutable release whose GitHub asset digests, schema-4
manifest, archive digest, and imported Incus fingerprint agree. The exact compatibility checks live
in [`internal/release/image.go`](../../internal/release/image.go). An existing exact fingerprint is
reused; a matching alias alone is not trusted.

Provider credentials remain in the host-side Provider Gateway. A Sandbox receives only its scoped
Codex configuration and revocable route capability at runtime; neither belongs to the image. See
[`docs/project/provider-gateway.md`](../project/provider-gateway.md) for the authority and credential
boundary.

The supported image is currently x86_64-only. Add another architecture only with its own built,
published, and real-Job validation terminal.
