# Getting started

Check [Support and diagnostics](support.md) before installing Dorf.

## 1. Install the application and initialize storage

Download the application archive and checksum from an immutable Dorf release, verify them, and put
`dorf` on `PATH`. Contributors building from source should use the repository-managed toolchain in
[CONTRIBUTING.md](../CONTRIBUTING.md).

Run the convergent setup entry point:

```bash
dorf setup
```

It prepares Docker/PostgreSQL first, then offers local Incus, cloud E2B, both, or neither. Selecting a
provider continues through Harness choice, ChatGPT-subscription or OpenAI-API authentication,
provider inputs, profile creation, functional verification, and default selection. E2B uses Dorf's
exact public Standard template build unless `--e2b-template` selects a custom exact build, and needs
one stable HTTPS `/v1` Gateway route. Setup can verify an existing route or guide a named Cloudflare
Tunnel after you authorize a hostname on a
Cloudflare-managed domain. Interactive setup discovers the hostname's DNS provider and offers the
guided Tunnel only when it finds Cloudflare nameservers and no existing address records; every
other domain stays on the existing-HTTPS-ingress path.

When supported Ubuntu 24.04 host changes are needed, setup previews and applies only those exact
changes after approval. `--yes` approves the same host and Cloudflare plans for automation; every
credential and provider choice must still be explicit in flags. `dorf setup --yes` alone prepares
only the common foundation.

Sign out and back in if setup adds Docker or Incus group access, then run the same command again.
Setup initializes a pristine Incus daemon only when Incus was selected and preserves operator-owned
storage and networking. It owns only the labeled `dorf-postgres` container and
`dorf-postgres-data` volume, exposes PostgreSQL on loopback, and never gives a Sandbox the host
Docker socket.

The separate `profile` and `provider` commands remain available for custom artifacts and advanced
operations. Their exact-artifact, credential, and route boundaries are described by the
[release process](releasing.md) and [Provider Gateway](project/provider-gateway.md).

## 2. Prove GitHub and repository authority

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

## 3. Run a coding Job

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
