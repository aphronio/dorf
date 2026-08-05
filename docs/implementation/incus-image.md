# Dorf Incus Image

This is implementation guidance for the credential-free `dorf-codex` VM image used by the
built-in Incus Environment. Room provisioning and Worker preparation must not install packages
from the network.

The default public path consumes a Dorf-built image. It includes Git, uv, Node, and Codex: the
complete ordinary coding workstation needed to clone a managed repository and run its declared
preparation and check commands without agent-led tool installation. Build-only npm is removed. The
local builder remains available for development:

```bash
scripts/incus/build-dorf-codex-image.sh
```

The build and provisioning scripts are the source of truth for the base fingerprint, installed
harness and tools, selected Codex release, and credential checks. `IMAGE_ALIAS` changes the local
published alias. The builder resolves the configured Ubuntu VM alias to an immutable fingerprint
before launching it and records the exact Codex version, npm package integrity, Git, Node, and uv
versions, and base fingerprint.

## Candidate and release pipeline

`scripts/incus/prepare-dorf-codex-release.sh` performs the complete candidate proof:

1. builds a uniquely named image from the current immutable base fingerprint;
2. resolves the current `@openai/codex@latest`, records its version and npm integrity, and installs
   that exact selected version;
3. launches a clean probe and checks the image for forbidden credentials and required tools;
4. clones Dorf at the release source commit and runs `.dorf.toml`'s deterministic preparation;
5. completes a real Codex implementation turn, the repo-owned checks, and a real Codex review with
   explicit access to the Room-scoped Provider Route;
6. records the image fingerprint, commands, preparation elapsed time, reviewer/route proof, and
   SHA-256-addressed command artifacts in a redacted local evidence directory;
7. verifies Room, workspace, runtime-state, and Provider Route removal; and
8. exports the VM, creates the canonical compatibility manifest, and reconciles its exact temporary
   VMs and candidate alias.

Run that proof manually with an already connected Provider Gateway name:

```bash
PROVIDER_CONNECTION=personal-chatgpt \
  scripts/incus/prepare-dorf-codex-release.sh
```

`scripts/incus/publish-dorf-codex-release.sh` runs that proof and publication as one repo-owned
local release operation. It keeps the selected Provider Gateway credential on the owner's host and
attaches only the credential-free archive and compatibility manifest to the same immutable `vX.Y.Z`
release that triggers the Python package publication. Run it from a clean versioned commit that has
already reached GitHub and passed the TestPyPI rehearsal:

```bash
PROVIDER_CONNECTION=personal-chatgpt \
  scripts/incus/publish-dorf-codex-release.sh
```

The x86_64 release assets are named `dorf-codex-incus-vm-x86_64.tar.gz` and
`dorf-codex-incus-vm-x86_64.json`. The explicit `incus-vm` segment prevents the release artifact
from looking like a generic Linux filesystem or a portable image for another Environment.

A complete draft containing the archive and manifest is published only when:

- the repository is public;
- GitHub immutable releases have been enabled;
- the repository variable `DORF_IMMUTABLE_RELEASES_ENABLED` records that reviewed setting;
- the named Provider Connection is valid on the local host; and
- the complete candidate proof passes.

The earlier lean `v0.1.1` release passed the core Worker proof on 2026-08-04 with Codex 0.146.0. Its
fresh probe contained Node and Codex but no Git, npm, or uv. Releases after the coding-workstation
slice add Git and uv while retaining the same credential-free boundary; npm remains absent. The
candidate terminal is now the clone-to-implementation-to-review proof above. The v0.1.1
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

`dorf.official_image.OfficialImageInstaller` is the setup-side convergence boundary. It accepts
only the newest non-draft, non-prerelease `vX.Y.Z` GitHub Release carrying a compatible manifest
when all of these agree:

- GitHub reports the release as immutable;
- GitHub's manifest asset digest matches the downloaded manifest;
- the manifest schema identifies the `incus` Environment, and its release tag, architecture, VM
  type, archive name, size, and digest match the release;
- the archive's GitHub digest, manifest digest, and Incus fingerprint are identical; and
- Incus reports the expected fingerprint after import.

An already-installed exact fingerprint is reused without downloading the archive. An old or custom
alias is never treated as current merely because its name matches. This consumer is intentionally
not exposed as a standalone public command: guided setup owns that call. Import and alias updates
use the commands available in Ubuntu 24.04's Incus 6.0; replacing an older alias does not delete its
underlying image because existing Rooms may still reference that fingerprint.

The current implementation is x86_64-only. Add another architecture only with its own built,
published, and real-Worker validation terminal.

## Credential boundary

Codex Rooms do not carry upstream credentials. A host-side Provider Gateway broker holds the
ChatGPT OAuth bundle or provider API key, and each sandbox receives exactly two derived inputs:

```text
/root/.codex/config.toml
/root/.config/dorf/provider-route.key
```

The private route-key file is read into `DORF_PROVIDER_ROUTE_KEY` only when Dorf launches
Codex. It is a revocable broker-local capability, not an upstream OAuth or API credential. See
`docs/project/model-auth-broker.md` and `docs/project/provider-gateway.md`. The host facade and
broker lifecycle shipped in #160, named Provider Connections in #161, and credential-free Room
wiring in #162.

The build script fails before publication if the seed contains `/root/.codex/auth.json`, a generated
Codex config, a Room route key, or ambient `OPENAI_API_KEY`. The release candidate repeats these
checks against a fresh launched VM before its real Worker terminal.

Historical secret-bearing Codex images are superseded and unsupported. Cloned login state proved
unreliable under concurrent refresh (#117, from #112/#114), which triggered D008's reconsideration
clause and led to D035. The public core image contains Codex, not Droid or workflow/reviewer tooling.
