# Releasing Dorf

From a clean source commit already available on GitHub, run the repository checks and the release
authority:

```bash
scripts/dev/setup.sh
.dorf/bin/mise run check
scripts/release.sh --publish
```

[`scripts/release.sh`](../scripts/release.sh) is the source of truth for release inputs, artifacts,
and publication. The release host must provide the repository toolchain installed by setup, a
working Docker CLI and Buildx builder, and authenticated GitHub and GHCR publication. The authority
rejects source changes before or during the build and verifies the release binary's Go VCS metadata
against that exact clean commit.

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

The authority reuses the exact pinned Incus image unless that image's declared inputs changed.
When the pin advances to the release being published, set `AI_CONNECTION` and
ensure the configured GitHub integration covers the source repository; the authority then invokes
the real Codex and Pi image proof before publication. Do not duplicate those details here or publish
by bypassing that command.
