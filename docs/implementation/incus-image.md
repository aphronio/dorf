# Dorf Incus Image

This is implementation guidance for the credential-free `dorf-codex` VM image used by the
Go application's Incus Sandbox. Keep this shared image stable instead of adding every managed
repository's development dependencies to it. After clone, deterministic repository preparation may
install that repository's pinned packages and tools from the network.

The default public path consumes a Dorf-built image. It includes Git, pinned Go 1.26.5, uv, Node,
and Codex: the existing baseline needed to clone a managed repository and run its deterministic
preparation command. Tools specific to Dorf itself, including Absurd and PostgreSQL, belong to
Dorf's repository-owned setup even when an existing image already happens to contain Go. Build-only
npm is removed. The local builder remains available for development:

```bash
scripts/incus/build-dorf-codex-image.sh
```

The build and provisioning scripts are the source of truth for the base fingerprint, installed
harness and tools, selected Codex release, and credential checks. `IMAGE_ALIAS` changes the local
published alias. The builder resolves the configured Ubuntu VM alias to an immutable fingerprint
before launching it and records the exact Codex version, npm package integrity, Git, Node, and uv
versions, the verified uv release-archive digest, and base fingerprint. Tool installation uses a
pinned x86_64 uv release archive whose published SHA-256 is verified before installation; it does
not execute a mutable network installer.

## Candidate and release pipeline

`scripts/incus/prepare-dorf-codex-release.sh` performs the complete candidate proof:

1. builds a uniquely named image from the current immutable base fingerprint;
2. resolves the current `@openai/codex@latest`, records its version and npm integrity, and installs
   that exact selected version;
3. launches a clean probe and executes the installed `go` and `gofmt`, matching `go version` to the
   image metadata, while checking the image for forbidden credentials and other required tools;
4. admits the release source commit through the Go durable Job spine, lets its Sandbox clone the
   repository, and requires the repository's declared preparation Action to succeed;
5. completes one real AgentRun through the Codex app-server Harness and a scoped Provider Route, with an explicit
   no-modification goal, and requires the exact starting Revision to remain current with the
   exact unchanged `git-revision` Evidence owned by that AgentRun and Revision generation zero;
6. records the image fingerprint, Harness/Thread/Turn identity, timings, and terminal state in a
   redacted local evidence directory;
7. verifies Sandbox and Provider Route cleanup in a `finally` boundary through the same durable Go
   path, with fenced Go cancellation and synchronous exact reconciliation as the failure fallback; and
8. exports the VM, creates the canonical compatibility manifest, and reconciles its exact temporary
   VMs and candidate alias.

This is deliberately a bounded image-capability proof, not the coding-to-PR terminal. Before
cleanup, the validator requires no repository-commit Action: implementation commits belong to the
AgentRun, and that Action kind no longer exists. The expected unchanged tree leaves the starting
Revision current at generation zero and records the unchanged observation after the completed
AgentRun, before any Checks, review AgentRuns, or proposal. Inspection derives that the Message was
handled without a committed change from the Evidence; it does not require a stored workflow phase
or attention. Retained evidence
labels Checks, review, and publication as not run or claimed. Evidence must name the exact AgentRun
and source Revision, so another Job's attention cannot satisfy the proof. The separate final cutover dogfood
owns exact-Revision Checks and Evidence, selected review, repair, publication, outcome, and cleanup.

Run that proof manually with an already connected Provider Gateway name:

```bash
PROVIDER_CONNECTION=personal-chatgpt \
  scripts/incus/prepare-dorf-codex-release.sh
```

If provider preflight fails, run the available Go readiness command from the Dorf source checkout:

```bash
go run ./cmd/dorf doctor --provider "$PROVIDER_CONNECTION"
```

`scripts/incus/publish-dorf-codex-release.sh` runs that proof and publication as one repo-owned
local release operation. It keeps the selected Provider Gateway credential on the owner's host and
attaches only the credential-free archive and compatibility manifest to the same immutable `vX.Y.Z`
release that contains the supported Go binary. Run it from a clean versioned commit that has
already reached GitHub and passed the Go checks:

```bash
PROVIDER_CONNECTION=personal-chatgpt \
  scripts/incus/publish-dorf-codex-release.sh
```

The schema-4 x86_64 release assets are named `dorf-codex-incus-vm-v4-x86_64.tar.gz` and
`dorf-codex-incus-vm-v4-x86_64.json`. Schema 4 is the first post-Go-cutover image contract. It
rejects the historical schema-3 workstation, which did not require Go and failed the issue #38
repository setup dogfood. The explicit `incus-vm` segment prevents the release artifact
from looking like a generic Linux filesystem or a portable image for another Environment. The
versioned channel keeps the Go application and its proven Sandbox image in one immutable boundary.

A complete draft containing the archive and manifest is published only when:

- the repository is public;
- GitHub immutable releases have been enabled;
- the repository variable `DORF_IMMUTABLE_RELEASES_ENABLED` records that reviewed setting;
- the named Provider Connection is valid on the local host; and
- the complete candidate proof passes.

The earlier lean `v0.1.1` release passed the historical core proof on 2026-08-04 with Codex 0.146.0. Its
fresh probe contained Node and Codex but no Git, npm, or uv. Releases after the coding-workstation
slice add Git and uv while retaining the same credential-free boundary; npm remains absent. The
issue-40 candidate terminal is the bounded durable clone-to-real-turn/no-change proof above.
Deterministic Checks and independent review are not run or claimed by the Go image proof; they
belong to the separate final cutover terminal. The v0.1.1
published archive is 765,823,845 bytes, about 52 MiB smaller than the previous workflow-tooling
image. Its archive and Incus fingerprint is
`0c269e0aa0c5a765e45bb50542b64d06e6c55930b920754459643991c7349775`; the manifest digest is
`93cbcd60b6af32b9cd1240c0813d4f43903cf22f36a93d661e6e0ea6c3d30ea3`, and both assets identify
release `v0.1.1` from source commit `0069db51d7e8b030501197b8ab89665575c15d84`.

The command then verifies the published release and both assets with GitHub CLI. Release
immutability protects the tag and assets after publication; GitHub records release attestations and
SHA-256 asset digests. See GitHub's [immutable release
model](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases)
and [release integrity
verification](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/secure-your-dependencies/verify-release-integrity).

The local publisher is intentionally manual for the first public releases. This avoids placing the
owner's ChatGPT subscription state in GitHub Actions and avoids a persistent self-hosted runner on a
public repository. Reconsider unattended publication only with a deliberately scoped non-personal
CI provider credential and an isolated runner. Do not advertise the official download before its
anonymous stranger terminal passes.

`EVIDENCE_POLICY=retain` is the candidate proof default and keeps redacted terminal evidence under
`dist/room-image/workstation-evidence`; `EVIDENCE_POLICY=remove` deletes it after validation. Neither
policy publishes that temporary evidence or writes it into the candidate image.

## Verified consumption

`dorf image install` is the Go setup-side convergence boundary. It accepts a matching immutable
release manifest and archive when all of these agree:

- GitHub reports the release as immutable;
- GitHub's manifest asset digest matches the downloaded manifest;
- manifest schema 4 identifies the `incus` Environment and complete Git, Go, Node, and uv workstation,
  and its release tag, architecture, VM type, archive name, size, and digest match the release;
- the archive's GitHub digest, manifest digest, and Incus fingerprint are identical; and
- Incus reports the expected fingerprint after import.

An already-installed exact fingerprint is reused without importing the archive. An old or custom
alias is never treated as current merely because its name matches. Import and alias updates
use the commands available in Ubuntu 24.04's Incus 6.0; replacing an older alias does not delete its
underlying image because existing Sandboxes may still reference that fingerprint.

The current implementation is x86_64-only. Add another architecture only with its own built,
published, and real-Job validation terminal.

## Credential boundary

Codex Sandboxes do not carry upstream credentials. A host-side Provider Gateway broker holds the
ChatGPT OAuth bundle or provider API key, and each sandbox receives exactly two derived inputs:

```text
/root/.codex/config.toml
/root/.config/dorf/provider-route.key
```

The private route-key file is read into `DORF_PROVIDER_ROUTE_KEY` only when Dorf launches
Codex. It is a revocable broker-local capability, not an upstream OAuth or API credential. See
`docs/project/model-auth-broker.md` and `docs/project/provider-gateway.md`. The host facade and
broker lifecycle shipped in #160, named Provider Connections in #161, and credential-free Sandbox
wiring in #162.

The build script fails before publication if the seed contains `/root/.codex/auth.json`, a generated
Codex config, a Sandbox route key, or ambient `OPENAI_API_KEY`. The release candidate repeats these
checks against a freshly launched VM before its real Job terminal.

Historical secret-bearing Codex images are superseded and unsupported. Cloned login state proved
unreliable under concurrent refresh (#117, from #112/#114), which triggered D008's reconsideration
clause and led to D035. The public core image contains Codex, not Droid or workflow/reviewer tooling.
