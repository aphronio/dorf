# Releasing Dorf

Choose `MAJOR.MINOR.PATCH` deliberately before publication. During the current `0.x` phase,
use a minor release for new capabilities or changed public/default behavior, and a patch release
for compatible fixes and maintenance. Reset the patch number to zero when advancing the minor.
The leading zero means the public contract is still evolving; `1.0.0` should mark an explicit
stability commitment, not a release-count milestone. See [Semantic Versioning](https://semver.org/).
The release version is owned by [`internal/version/version.go`](../internal/version/version.go).

Commit the chosen version to `main` (normally through a pull request). After push CI succeeds,
the Release workflow automatically checks that exact tested commit and publishes its version if
no GitHub release with that tag exists. Unchanged, already released versions are skipped, including
CI reruns. Failed CI and pull-request CI cannot publish. A draft release also prevents automatic
republication; inspect and resolve an interrupted publication before retrying.

CI owns the blocking SQL, Go, and PostgreSQL-backed checks; documentation validation remains an
explicit local check. After database initialization, code checks and release artifact construction
run concurrently; both must succeed before installer and
artifact smoke checks proceed. Publication remains serialized, installs only the locked Go
toolchain, and requires successful push CI for the exact selected commit. Manual dispatch remains
available for retries from a clean commit on `main` already available on GitHub:

```bash
gh workflow run release.yml --ref main
```

CI and publication share Go module, compilation, and container-layer caches. Cache keys include the
locked toolchain, Go dependencies, and container recipe; each new source commit can restore a prior
compatible cache. Builds still verify clean source and binary provenance. Local container builds
can opt into the same layer reuse by setting `DORF_BUILDX_CACHE` to a dedicated disposable cache
directory; the builder replaces that directory after a successful image build.

GitHub release immutability must already be enabled and the repository variable
`DORF_IMMUTABLE_RELEASES_ENABLED` must record `true`. The existing `dorf` GHCR package must grant
this repository Write access under **Manage Actions access**; the image's OCI source label keeps the
package linked to the repository after publication.

[`scripts/release.sh`](../scripts/release.sh) remains the source of truth for release inputs,
artifacts, and publication. Hosted Actions supplies the locked repository toolchain,
Docker/Buildx, and narrowly scoped `GITHUB_TOKEN` GitHub and GHCR publication permissions. It does
not provision a development database or repeat CI. The workflow checks out the exact selected commit;
the authority rejects source changes before or during the build and verifies the release binary's
Go VCS metadata against that commit.

Every run builds one x86_64 Linux application archive and the Linux/amd64 image
`ghcr.io/aphronio/dorf:MAJOR.MINOR.PATCH` from the same exact binary and canonical container recipe.
Publishing pushes that exact semantic-version image to GHCR; it does not create a Docker image tar,
load cache, OCI parser, or second release transport. The application archive contains:

- `dorf`;
- the static `dorf-compose.yaml` and `dorf-compose-incus.yaml` manifests;
- the inspectable `bootstrap/docker.sh`, `bootstrap/incus.sh`, and `bootstrap/incus-remote.sh`
  administrator helpers; and
- the license.

The container recipe pins its Dockerfile frontend, Debian image, and Debian package snapshot. It
removes package-manager logs and machine-specific loader cache data, normalizes layer timestamps to
a fixed epoch, and keeps the application binary in an independently reusable layer. The binary's
Go VCS metadata remains the source-commit identity. Rebuilding the same release inputs therefore
produces the same image digest without making unchanged runtime layers commit-specific.

The checksum file identifies the application archive exactly once. The installer verifies the
complete set before replacing each installed file atomically. It places the binary, both manifests,
and the remote Incus helper beside one another. The manifests select the published semantic-version
image with an always-pull policy; operator lifecycle remains the direct Compose procedure in [Getting
started](getting-started.md#1-install-the-application-initialize-a-deployment-host).

Publication first prepares the image and application archive without changing GitHub's `latest`
release, verifies the signed immutable release and every uploaded asset, and only then promotes it
to `latest`. A failed verification leaves the prior latest release unchanged.

The hosted workflow accepts only a reused, already published Incus image pin. When the pin
advances, run `scripts/release.sh --publish` locally with a working Incus endpoint. The authority
builds the pinned guest recipe, exports its image, validates the archive and version metadata,
and removes its temporary build VM and image alias. Publication does not require a Dorf
deployment, AI connection, GitHub App installation, coding Job, or browser navigation proof.
GitHub and GHCR publication credentials are still required.

Browser packages and Chromium live in the shared Nix workstation recipe. Image metadata records
their versions and immutable workstation identity. Deployment profile verification remains a
separate admission requirement. Use the repository release command for both paths.

## Local release benchmark

Run the real release builder and existing installer/authority proofs without publishing or
triggering GitHub Actions:

```bash
mise run release:benchmark -- --runs 3 --output .dorf/release-benchmarks/baseline
# Compare only explicitly selected tracked working-tree changes against the same base.
mise run release:benchmark -- --runs 3 --output .dorf/release-benchmarks/candidate \
  --overlay scripts/build-release.sh --overlay scripts/release-test.sh
```

Direct invocation is `bash scripts/benchmark-release.sh` with the same arguments. The default is
three runs from `HEAD`; `--ref REVISION` selects another local commit. `--output` must be a new
directory beneath this repository's ignored `.dorf/`; omitting it creates a timestamped directory
under `.dorf/release-benchmarks/`. Use the same explicit `--ref` for both comparisons if HEAD may
change between them. `--runs 1` measures cold only.

The driver clones committed source without hard links and trusts the clone's Mise configuration,
then installs its locked Go toolchain. It never snapshots unrelated working-tree changes. Repeat
`--overlay` for existing tracked regular files to copy into a clean, signed-off synthetic local
commit **only in the disposable clone**. Overlay paths must also exist in the selected revision;
new files, deletions, symlinks and directory overlays are unsupported. The original checkout and
its index are unchanged, and the retained clone has no remote. Normal release source-cleanliness
and binary-provenance checks remain enabled.

Run 1 starts with empty receipt-local `GOCACHE`, `GOMODCACHE` and external Buildx layer caches.
Later runs reuse those caches, but every run creates and bootstraps a new uniquely named
`docker-container` builder, importing the previous exported cache rather than retaining a builder's
internal state. Each run builds real local artifacts, passes the generated installer and archive
to `scripts/install-test.sh`, and runs `scripts/release-test.sh`. The authority test retains its
existing fixture-based design; it does not replace the real build. Builders are removed on success,
failure or a handled interrupt; no shared Docker builders, images or caches are pruned.

Receipts retain `steps.tsv` (monotonic wall elapsed seconds and exit status per step),
`cache-sizes.tsv`, source commit and overlay patch, host/tool/environment metadata, cache contents,
per-run logs and artifacts under `runs/`, and `result.json`. Failures stop the loop, return nonzero
and preserve the receipt and failing log. Tool installation and builder setup are timed separately
from the build and proofs. The existing installer proof requires Python 3 and curl in addition to
Linux Bash, Git, Mise, Docker/Buildx, jq and the usual release command-line tools.

This is a local performance comparison, not a hosted-runner latency prediction: host resources,
contention and filesystem differ, Mise and Docker daemon downloads can already be cached, and
local cache access does not measure GitHub cache transfer. The no-publish builder exports/loads
an image and verifies it locally instead of pushing to a registry. Its semantic-version image is
left in the local Docker daemon and can replace that local tag; do not run concurrent comparisons
against the same daemon. No GitHub or registry publication credentials are needed.

## Shared guest packages

Both builders consume the shared Debian guest recipe and
[`scripts/sandbox/packages`](../scripts/sandbox/packages). That directory pins Nix, Nixpkgs, and
the official prebuilt Codex archives. Preserve the complete upstream platform directory, including
companion executables and runtime resources; a successful `codex --version` does not prove native
tools can start. Codex uses the `dorf-runner` Nix profile; the remaining tools,
Pi, and browser environment share `dorf-tools`. Their separation allows the verified Codex update
to preserve the rest of the workstation. The guest includes `dorf-packages` for staging supported
pinned Codex versions with Internet access. Image metadata retains actual tool versions, Harness
archive identities, and the workstation's store path and Nixpkgs provenance. Nix generations do not
replace VM-state checkpoints. No browser is started or managed by Dorf. Browser-use talks directly
to Chromium through CDP; Playwright is not installed. The upstream browser-use installer supplies
its skill unchanged, with no Dorf-specific text inserted.

For disposable local candidate builds and retained-conversation verification, use
`mise run integration:nix-image build incus` or `mise run integration:nix-image build e2b`.
Each prints the exact `verify` command and retains a build receipt with input hashes. The recipe
allows an explicitly recorded dirty source tree for iteration; it does not publish an official
release, promote a deployment profile, or update an existing user VM. Verify both candidates
sequentially against the configured disposable PostgreSQL database. The verification uses the
image's installed package helper and requires baked-in Nix before staging additional versions.
The workstation probe compiles native code, creates a Python environment, and exercises browser-use
through an agent-started Chromium process. Its retained `workstation.json` allows exact package
parity comparison across providers.

Use `python3 scripts/sandbox/packages/lock.py browser` or `pi` on a development machine to refresh
the browser wheel inputs or Pi dependency lock. Review those changes and update Pi's `npm_deps_hash`
from the Nix fetcher's reported hash when its dependency lock changes. Both provider builders must
then pass the same fresh-image proof. Do not run workstation installation as an uncoordinated live
upgrade on a retained user VM; the current recovery executor is scoped to Codex.

## E2B template

The E2B builder uses the shared guest recipe from a clean source commit. It loads
`E2B_API_KEY` through Bun from the repository-root `.env`; an exported value takes
precedence. Do not infer missing credentials from the calling shell alone. Check
configuration without starting a paid build or displaying the key:

```bash
scripts/e2b/build-template.sh --check
```

Build the template with `scripts/e2b/build-template.sh`. The builder writes its exact
reference and recipe provenance to `dist/e2b-template/profile.json`.
Verify that build with `DORF_E2B_PROFILE_LIVE=1`, `E2B_API_KEY`, and
`DORF_E2B_PROFILE_MANIFEST` pointing to the manifest, using
`mise exec -- go test ./internal/e2b -run '^TestLiveCombinedHarnessProfile$' -count=1`.
The Go test requires the key in its environment; it does not load `.env` itself.

Private templates are scoped to the E2B team used for the build. Build with the target
deployment team's credentials, or explicitly authorize public release of a reviewed clean
template. A successful local build does not establish that another team can use it. Verify the
exact reference using the deployment credentials before promoting the profile; failed verification
must leave the previously active revision available. Keep credentials in the build client,
never in template commands or copied build inputs.
