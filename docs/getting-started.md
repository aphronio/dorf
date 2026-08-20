# Getting started

Check [Support and diagnostics](support.md) before installing Dorf.

## 1. Install the application and initialize storage

Download the application archive and checksum from an immutable Dorf release, verify them, and put
`dorf` on `PATH`. Contributors building from source should use the repository-managed toolchain in
[CONTRIBUTING.md](../CONTRIBUTING.md).

Run the convergent setup entry point. It observes the host first. When supported Ubuntu 24.04 host
changes are needed, the interactive prompt lists only those exact changes and applies them after
approval. Automation may approve the same observed plan explicitly:

```bash
dorf setup
dorf setup --yes
```

Sign out and back in if setup adds Docker or Incus group access, then run the same command again.
Setup initializes only a pristine Incus daemon and preserves operator-owned storage and networking.
It owns only the labeled `dorf-postgres` container and `dorf-postgres-data` volume, exposes
PostgreSQL on loopback, and never gives a Sandbox the host Docker socket.

## 2. Install a Sandbox profile

Use the same release tag as the application:

```bash
dorf profile install local-codex --release vX.Y.Z --harness codex
dorf profile verify local-codex
dorf profile set-default local-codex
```

For an offline-prepared host, download the manifest and archive from that release and pass them
explicitly:

```bash
dorf profile install local-codex \
  --manifest MANIFEST.json --archive IMAGE.tar.gz --harness codex
```

The CLI verifies release and image identity before import, then stores the exact fingerprint. The
explicit profile verification creates one disposable Sandbox, runs Dorf's base functional probe,
deletes it, and confirms absence before the profile can admit work. To bring an existing artifact,
use `dorf profile create` with its provider-specific image or template reference. Image construction,
publication, and consumer validation are owned by the repository's release command; see the
[release process](releasing.md).

## 3. Connect the provider and initialize Dorf

```bash
dorf provider connect chatgpt --name personal-chatgpt --profile local-codex
dorf setup --provider personal-chatgpt --profile local-codex
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
  --profile local-codex \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --github-repo OWNER/REPOSITORY \
  --github-installation INSTALLATION_ID \
  --base main
```

Every failed fact includes a remediation.

## 5. Run a coding Job

The selected profile owns the Harness. Omit `--profile` to use the verified deployment default.
Create and verify a separate Pi profile when that Job should use Pi; both may reference the same
exact credential-free image.

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

To follow the same durable facts without repeatedly invoking inspection, use:

```bash
dorf inspect --follow JOB_ID
```

The follower shows current status and durable Job history until attention appears or cleanup
completes. `Ctrl-C` stops only the local view, not the Job.

`worker` may be restarted after process loss. Use `dorf message` for later input and `--intent steer`
to target active work. The coding workflow observes the exact pull request for acceptance or
rejection and requests cleanup after its terminal policy is satisfied. To stop without a GitHub
decision:

```bash
dorf abandon JOB_ID
dorf inspect JOB_ID
```

If `dorf inspect JOB_ID` reports that the workflow stopped, repair the displayed cause and run
`dorf retry JOB_ID`. This schedules exactly one more bounded attempt on the same Absurd task and
retains its checkpoints. The receipt reports scheduling identities but does not claim that a worker
has resumed it yet; use `dorf inspect JOB_ID` to observe current work and progress.

Cleanup remains separately observable. `dorf cleanup JOB_ID` is an explicit client request to release
the Job's resources; Core reconciles that request or retries an incomplete cleanup, then inspection
reports the resulting facts.
