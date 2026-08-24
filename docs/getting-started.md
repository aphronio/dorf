# Getting started

Check [Support and diagnostics](support.md) before installing Dorf.

## 1. Install the application and initialize storage

Beginning with the first release after `v0.3.0`, install the latest immutable Dorf release:

```bash
curl -fsSL https://github.com/aphronio/dorf/releases/latest/download/install.sh | sh
```

The installer downloads the matching x86_64 Linux archive and checksum, verifies the archive before
atomically installing `dorf` to `~/.local/bin`, and prints a `PATH` handoff when needed. It does not
run setup. Upgrade an installed binary through the same verified path with `dorf update`. Install an
exact release by using its pinned installer asset:

```bash
RELEASE_TAG=YOUR_RELEASE_TAG
curl -fsSL "https://github.com/aphronio/dorf/releases/download/$RELEASE_TAG/install.sh" | sh
```

Release `v0.3.0` predates the installer asset. For that release, or when independently verifying
GitHub's signed release attestation is required, use the transparent manual path:

```bash
RELEASE_TAG=v0.3.0
release_dir="$(mktemp -d)"
gh release verify "$RELEASE_TAG" --repo aphronio/dorf
gh release download "$RELEASE_TAG" --repo aphronio/dorf --dir "$release_dir" \
  --pattern "dorf_${RELEASE_TAG#v}_linux_x86_64.tar.gz" \
  --pattern "dorf_${RELEASE_TAG#v}_checksums.txt"
(cd "$release_dir" && sha256sum --check "dorf_${RELEASE_TAG#v}_checksums.txt")
tar -xzf "$release_dir/dorf_${RELEASE_TAG#v}_linux_x86_64.tar.gz" -C "$release_dir"
sudo install -m 0755 "$release_dir/dorf" /usr/local/bin/dorf
dorf version
```

Contributors building from source should instead use the repository-managed toolchain in
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
changes after approval. `--yes` approves the same host and Cloudflare plans for automation. Setup
reuses one unambiguous ready AI connection when it already exists; otherwise automation must name
one with `--ai-connection` or explicitly select its authentication mode. Sandbox provider choices
remain explicit. `dorf setup --yes` alone prepares only the common foundation.

Sign out and back in if setup adds Docker or Incus group access, then run the same command again.
Setup initializes a pristine Incus daemon only when Incus was selected and preserves operator-owned
storage and networking. It owns only the labeled `dorf-postgres` container and
`dorf-postgres-data` volume, exposes PostgreSQL on loopback, and never gives a Sandbox the host
Docker socket.

The separate `profile` and `provider` commands remain available for custom artifacts and advanced
operations. Their exact-artifact, credential, and route boundaries are described by the
[release process](releasing.md) and [Provider Gateway](project/provider-gateway.md).

## 2. Set up the optional GitHub integration

Skip this section when a client or workflow needs only plain Git access, such as cloning a public
repository or consuming retained Git input. A GitHub App is required for authenticated GitHub API
or repository operations.

Create the deployment-default App through GitHub's approval flow:

```bash
dorf integration github setup
```

Use `--org OWNER` when the organization should own the App; omit it for an App owned by the
authenticated GitHub user. Setup prints a readable HTTPS link to Dorf's static GitHub Pages
launcher and an explicit copy-and-paste fallback. The page has no backend, tracking, or callback; it
submits the fixed App manifest directly to GitHub, then displays GitHub's returned one-time code
with a Copy button. After approving GitHub's form, copy that code into the waiting command.
Dorf exchanges it, verifies the returned App identity and exact supported permission envelope,
atomically installs GitHub's returned credential bundle, and prints `GitHub App created`. Setup then
prints the reusable App installation URL. Open it, install the App with access to at least one
repository, return to the waiting command, and type `installed`. Dorf makes one authenticated
observation through the App authority and prints `GitHub integration ready` only after GitHub reports
at least one installation.

The App registration uses the fixed module permission envelope owned by
[D093](project/decisions.md#d093--github-authentication-is-an-optional-deployment-integration).
Runtime operations still mint repository-scoped tokens with only their exact required subset. Setup
runs no local callback listener or hosted relay and does not select, poll, or verify a repository.
Repeating setup remotely proves the configured App identity, permission envelope, and presence of an
installation. An already installed App returns ready without reading terminal input; an App with no
installation resumes at the same reusable installation URL instead of creating another App.
Replacing the configured credential bundle retains its explicit `--yes` approval boundary.

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
  --base main \
  --model MODEL \
  --reasoning high

dorf worker
dorf inspect JOB_ID
```

Admission derives the exact GitHub owner/repository from `--repo`. The coding runtime composed for
the selected profile discovers the deployment-default App installation before admitting a new Job;
the Job request carries no integration or permission settings. The caller supplies the exact starting
`--revision` and base. Retrying an existing key reuses its retained installation so recovery does not
depend on GitHub availability; the complete caller input must still match.

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
