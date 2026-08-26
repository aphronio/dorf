# Releasing Dorf

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
artifacts, and publication. Hosted Actions supplies the locked Go toolchain, Docker/Buildx, and
narrowly scoped `GITHUB_TOKEN` GitHub and GHCR publication permissions. It does not provision a
development database or repeat CI. The workflow checks out the exact event commit; the authority
rejects source changes before or during the build and verifies the release binary's Go VCS metadata
against that commit.

Every run builds one x86_64 Linux application archive and the Linux/amd64 image
`ghcr.io/aphronio/dorf:MAJOR.MINOR.PATCH` from the same exact binary and canonical container recipe.
Publishing pushes that exact semantic-version image to GHCR; it does not create a Docker image tar,
load cache, OCI parser, or second release transport. The application archive contains:

- `dorf`;
- the static `dorf-compose.yaml` and `dorf-compose-incus.yaml` manifests;
- the inspectable `bootstrap/docker.sh` and `bootstrap/incus.sh` administrator helpers; and
- the license.

The checksum file identifies the application archive exactly once. The installer verifies the
complete set before replacing each file atomically beside the others. The manifests select the published
semantic-version image with an always-pull policy; operator lifecycle remains the direct Compose
procedure in [Getting started](getting-started.md#1-install-the-application-initialize-a-deployment-host).

Publication first prepares the image and application archive without changing GitHub's `latest`
release, verifies the signed immutable release and every uploaded asset, and only then promotes it
to `latest`. A failed verification leaves the prior latest release unchanged.

The hosted workflow accepts only a reused, already proven Incus image pin. When the pin advances,
publication remains local: set `AI_CONNECTION`, ensure the configured GitHub integration covers the
source repository, and run `scripts/release.sh --publish` so the authority performs the real Codex
and Pi image proof before publication. Provider credentials do not move to hosted Actions. Do not
bypass the repository command for either path.
