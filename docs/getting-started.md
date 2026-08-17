# Getting started

Check [Support and diagnostics](support.md) before installing Dorf.

## 1. Install the application and host services

Download the application archive and checksum from an immutable Dorf release, verify them, and put
`dorf` on `PATH`. Contributors building from source should use the repository-managed toolchain in
[CONTRIBUTING.md](../CONTRIBUTING.md).

On the validated clean-machine host, review and apply the native installation plan:

```bash
dorf host install
dorf host install --yes
```

Sign out and back in if requested, then rerun the command. Dorf only initializes a pristine Incus
daemon; preserve operator-owned storage and networking.

Set the deployment database when the default local PostgreSQL DSN is not appropriate:

```bash
export DORF_DATABASE_URL='postgresql:///dorf?host=/var/run/postgresql'
```

## 2. Install the Sandbox image

Use the same release tag as the application:

```bash
dorf image install --release vX.Y.Z
```

For an offline-prepared host, download the manifest and archive from that release and pass them
explicitly:

```bash
dorf image install --manifest MANIFEST.json --archive IMAGE.tar.gz
```

The CLI verifies release and image identity before import. Image construction, publication, and
consumer-validation authorities are indexed in the [Incus image guide](implementation/incus-image.md).

## 3. Connect the provider and initialize Dorf

```bash
dorf provider connect chatgpt --name personal-chatgpt
dorf setup --provider personal-chatgpt
```

Provider state is deployment-owned and defaults under the XDG data directory. Override its location
with `DORF_PROVIDER_GATEWAY_STATE` when needed. See the [Provider Gateway](project/provider-gateway.md)
for its credential and route boundary.

## 4. Prove GitHub and repository authority

Configure a GitHub App with metadata-read, issues-read, contents-write, and pull-requests-write
access to the selected repository. Keep its metadata and private key at the paths reported by
`dorf doctor`, or set `DORF_GITHUB_APP_METADATA` and `DORF_GITHUB_APP_PRIVATE_KEY`.

```bash
dorf doctor \
  --provider personal-chatgpt \
  --contract .dorf.toml \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --github-repo OWNER/REPOSITORY \
  --github-installation INSTALLATION_ID \
  --base main
```

Every failed fact includes a remediation.

## 5. Run a coding Job

Codex is the default Harness. To use Pi, export `DORF_HARNESS=pi` for the Dorf commands and Worker.
Both Harnesses use the same installed credential-free image.

Save the complete goal in `goal.txt`, then admit it with stable authority:

```bash
dorf admit \
  --key my-change-v1 \
  --goal-file goal.txt \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --revision FULL_COMMIT_OID \
  --branch dorf/my-change-v1 \
  --github-repo OWNER/REPOSITORY \
  --github-installation INSTALLATION_ID \
  --base main \
  --provider personal-chatgpt \
  --model MODEL \
  --reasoning high

dorf worker
dorf inspect JOB_ID
```

`worker` may be restarted after process loss. Use `dorf message` for later input and `--intent steer`
to target active work. Dorf observes the exact pull request for acceptance or rejection. To stop
without a GitHub decision:

```bash
dorf abandon JOB_ID
dorf inspect JOB_ID
```

If `dorf inspect --json JOB_ID` reports the attached main task as `failed` after its underlying cause
has been repaired, run `dorf retry JOB_ID`. This schedules exactly one more bounded attempt on the
same Absurd task and retains its checkpoints. The receipt reports scheduling identities but does not
claim that a worker has resumed it yet; use `dorf inspect JOB_ID` to observe current work and progress.

Cleanup remains separately observable. Use `dorf cleanup JOB_ID` only to explicitly start or retry
it, then inspect again.
