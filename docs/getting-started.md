# Getting started on x86_64 Linux

The supported host is x86_64 Linux with hardware virtualization and a local Incus daemon. Ubuntu
24.04 is the clean-machine path currently validated. macOS is unsupported because it cannot host
the local Incus VM daemon; running only the CLI on macOS does not create a supported remote mode.

## 1. Install the Go application and native services

Download `dorf_VERSION_linux_x86_64.tar.gz` and its checksum from the same immutable GitHub release
as the Incus image, verify it, and install `dorf` on `PATH`. Building the same artifact requires Go
1.25 or newer:

```bash
go build -o ./bin/dorf ./cmd/dorf
```

On Ubuntu 24.04, Dorf can apply the reviewed native package/service recipe after first displaying
its exact administrator and root-equivalent group effects:

```bash
dorf host install
dorf host install --yes
```

Sign out and back in if requested, then repeat the same command. It initializes only a pristine
Incus daemon and creates the local PostgreSQL role/database idempotently. The resulting default DSN
is:

```bash
export DORF_DATABASE_URL='postgresql:///dorf?host=/var/run/postgresql'
```

Do not run `incus admin init --minimal` over a partially configured daemon: inspect and preserve
operator-owned storage and networking instead.

## 2. Install the credential-free Codex image

Install directly from the same immutable Dorf release tag used for the Go binary:

```bash
dorf image install --release v0.2.0
```

The Go CLI requires GitHub to report the release immutable, downloads exactly these two assets,
verifies both GitHub SHA-256 digests plus the manifest/archive agreement, and then verifies the
imported Incus fingerprint:

```text
dorf-codex-incus-vm-v4-x86_64.tar.gz
dorf-codex-incus-vm-v4-x86_64.json
```

An offline-prepared host can download those assets separately and use the equivalent local path:

```bash
dorf image install \
  --manifest dorf-codex-incus-vm-v4-x86_64.json \
  --archive dorf-codex-incus-vm-v4-x86_64.tar.gz
```

Maintainers can instead build and prove the image locally with
`scripts/incus/release-dorf-codex-image.sh`. Schema 4 is the first post-cutover image contract: it
identifies an exact Debian 13 base and contains Python 3.14, Node 24 LTS, pinned Go and uv, Codex,
native build tools, and common command-line utilities. It proves the managed repository's declared
preparation inside a fresh Sandbox before publication. Repository libraries and test tools remain
lockfile-owned and are installed by that preparation command. Schema-3 images are pre-cutover and
unsupported even if their alias is still present locally. The image contains no upstream credential
or scoped route key.

## 3. Connect the provider and initialize PostgreSQL/Absurd

The supported ChatGPT-subscription route uses the pinned CLIProxyAPI broker. This is a separate
concrete Go binary and the only retained helper service in Dorf's model path. Dorf downloads its
verified x86_64 Linux release, binds it to the private Incus bridge, and launches its device login:

```bash
dorf provider connect chatgpt --name personal-chatgpt
dorf setup --provider personal-chatgpt
```

Dorf keeps this deployment-owned provider data under the XDG data directory by default
(`$XDG_DATA_HOME/dorf/provider-gateway`, or `~/.local/share/dorf/provider-gateway`). An operator may
override that location with `DORF_PROVIDER_GATEWAY_STATE`; it is never stored in a Job.

`setup` downloads the immutable Absurd 0.5.0 schema only for first initialization, verifies its
hard-coded SHA-256, applies the embedded Dorf schema, and runs bounded direct checks. A prepared
offline machine may pass `--absurd-schema FILE`.

## 4. Prove GitHub and repository authority

Configure a GitHub App with metadata-read, issues-read, contents-write, and pull-requests-write authority for
the selected repository. Keep its metadata and private key at the paths shown by `dorf doctor` (or
set `DORF_GITHUB_APP_METADATA` and `DORF_GITHUB_APP_PRIVATE_KEY`). Then run:

```bash
dorf doctor \
  --provider personal-chatgpt \
  --contract .dorf.toml \
  --repo https://github.com/aphronio/dorf.git \
  --github-repo aphronio/dorf \
  --github-installation INSTALLATION_ID \
  --base greenfield
```

Every failed fact includes concrete remediation. The command checks PostgreSQL, Absurd and its
queue, Incus access/network/image, provider route authority, the Go-first repository contract, and
the exact GitHub App repository/base authority. It never probes Docker.

## 5. Run a coding Job

Save the complete goal in `goal.txt`, then admit it with stable authority:

```bash
dorf admit \
  --key my-change-v1 \
  --goal-file goal.txt \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --revision FULL_COMMIT_OID \
  --branch dorf/my-change-v1 \
  --github-repo owner/repository \
  --github-installation INSTALLATION_ID \
  --base main \
  --provider personal-chatgpt \
  --model gpt-5.6-sol \
  --reasoning high

dorf worker
dorf inspect JOB_ID
```

`worker` may be restarted after process loss. Send a stable message while work is active with
`dorf message`; use `--intent steer` to target the current native turn. Dorf observes merge or close
on the exact pull request and records acceptance or rejection automatically. To stop without a
GitHub decision, explicitly abandon the Job:

```bash
dorf abandon JOB_ID
dorf inspect JOB_ID
```

Cleanup remains a separate observable lifecycle fact and follows a terminal Outcome automatically.
Use `dorf cleanup JOB_ID` only when an operator must explicitly start or retry cleanup, then inspect
again.
