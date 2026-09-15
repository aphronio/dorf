# Releasing Dorf

Choose `MAJOR.MINOR.PATCH` deliberately before publication. During the current `0.x` phase,
use a minor release for new capabilities or changed public/default behavior, and a patch release
for compatible fixes and maintenance. Reset the patch number to zero when advancing the minor.
The leading zero means the public contract is still evolving; `1.0.0` should mark an explicit
stability commitment, not a release-count milestone. See [Semantic Versioning](https://semver.org/).
The release version is owned by [`internal/version/version.go`](../internal/version/version.go).

From a clean commit on `main` already available on GitHub with successful CI, dispatch the Release
workflow. CI owns the full repository and PostgreSQL-backed checks. The publication workflow installs
only the pinned release toolchain, requires a successful push CI run for the exact event commit, and
invokes the release authority on that commit:

```bash
gh workflow run release.yml --ref main
```

GitHub release immutability must already be enabled and the repository variable
`DORF_IMMUTABLE_RELEASES_ENABLED` must record `true`. The existing `dorf` GHCR package must grant
this repository Write access under **Manage Actions access**; the image's OCI source label keeps the
package linked to the repository after publication.

[`scripts/release.sh`](../scripts/release.sh) remains the source of truth for release inputs,
artifacts, and publication. Hosted Actions supplies the locked repository toolchain,
Docker/Buildx, and narrowly scoped `GITHUB_TOKEN` GitHub and GHCR publication permissions. It does
not provision a development database or repeat CI. The workflow checks out the exact event commit;
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

## Shared guest packages

Both builders consume the shared Debian guest recipe and
[`scripts/sandbox/packages`](../scripts/sandbox/packages). That directory pins Nix, Nixpkgs, and
the official prebuilt Codex archives. Codex uses the `dorf-runner` Nix profile; the remaining tools,
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
