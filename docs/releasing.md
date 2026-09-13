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

The hosted workflow accepts only a reused, already proven Incus image pin. When the pin advances,
publication remains local: set `AI_CONNECTION` and `PROOF_PROFILE` to a ready connection and verified
Incus profile, ensure the configured GitHub integration covers the source repository, and run
`scripts/release.sh --publish`. The local CLI must be connected to that deployment's Control API.
The local Incus endpoint must be the profile's endpoint; the authority copies the candidate into
the profile's project and creates separate temporary proof profiles. When the deployment host is
remote, set `DORF_HOST_COMMAND` to an executable wrapper that forwards its arguments to `dorf` on
that host. Profile creation and verification use this wrapper; Job operations use the connected
local CLI and the deployment's running worker. `PROOF_MODEL` optionally overrides the proof model.

The authority requires real Codex and Pi no-change coding turns, unchanged Revision Evidence,
browser navigation through the preinstalled CLI, and completed Sandbox cleanup before publication.
Browser packages and Chromium live in the Incus-specific recipe; the shared E2B guest recipe is
unchanged. Image metadata records the browser package, Python, Playwright, and Chromium versions.
Provider credentials do not move to hosted Actions. Do not
bypass the repository command for either path.

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
