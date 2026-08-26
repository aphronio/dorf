# Releasing Dorf

From a clean source commit already available on GitHub, run the repository checks and the release
authority:

```bash
scripts/dev/setup.sh
.dorf/bin/mise run check
scripts/release.sh --publish
```

[`scripts/release.sh`](../scripts/release.sh) is the source of truth for release inputs, artifacts,
and publication. The release host must provide the repository toolchain installed by setup, plus a
working Docker CLI and Buildx builder. The authority rejects source changes before or during the
build and verifies the release binary's Go VCS metadata against that exact clean commit. Every run
builds the application archive and a Linux/amd64 Docker image archive from the same exact release
binary and the canonical release container recipe. The application archive also carries the exact
reviewed `bootstrap/docker.sh` and `bootstrap/incus.sh` administrator helpers embedded in that
binary, plus the reviewed `bootstrap/retire-systemd.sh` one-time deployment-migration helper, so
humans can inspect or run the same bytes Dorf materializes during an administrator handoff. The
image archive is named
`dorf_<version>_linux_x86_64_container-image.docker.tar`, embeds
`ghcr.io/aphronio/dorf:<version>`, and is directly loadable with `docker image load`. The one release
checksum file identifies the application archive and container image archive exactly once each. The
producer temporarily loads the archived
image by its configuration digest, proves the exact binary under an unprivileged, networkless,
read-only container, and removes only that attested image reference. Publication first makes the
release immutable without changing `latest`, verifies the signed release and every asset, and only
then promotes it to `latest`; a failed verification leaves the prior latest release unchanged.

The authority reuses the exact pinned Incus image unless that image's declared inputs changed.
When the pin advances to the release being published, set `AI_CONNECTION` and
ensure the configured GitHub integration covers the source repository; the authority then invokes
the real Codex and Pi image proof before publication. Do not duplicate those details here or publish
by bypassing that command.
