# Releasing Dorf

Dorf releases one x86_64 Linux Go application and the credential-free Incus VM image proven with
that application. There is no Python package or package-index publication.

From one clean source commit already available on GitHub:

```bash
go test ./...
go vet ./...
scripts/build-release.sh dist/room-image

PROVIDER_CONNECTION=personal-chatgpt \
GITHUB_INSTALLATION_ID=INSTALLATION_ID \
  scripts/incus/publish-dorf-codex-release.sh
```

The local publisher retains the owner's provider credential on the host. It builds a fresh image,
checks the credential boundary, admits a real Go Job against the exact source commit, completes a
real Codex turn, proves exact cleanup, builds the static Go archive/checksum, creates one complete
draft containing all four assets, and publishes it once. GitHub release immutability and `gh
release verify[-asset]` are required.

Release tags are exactly `v` plus `internal/version.Version`. The assets are:

```text
dorf_VERSION_linux_x86_64.tar.gz
dorf_VERSION_checksums.txt
dorf-codex-incus-vm-v4-x86_64.tar.gz
dorf-codex-incus-vm-v4-x86_64.json
```

Do not publish from a dirty checkout, retag a different Revision, upload provider state, or publish
the draft before the real Go terminal and cleanup pass.
