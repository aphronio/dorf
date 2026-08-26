# Getting started

Check [Support and diagnostics](support.md) before installing Dorf.

## 1. Install the application; initialize a deployment host

Beginning with the first release after `v0.3.0`, install the latest immutable Dorf release:

```bash
curl -fsSL https://github.com/aphronio/dorf/releases/latest/download/install.sh | sh
```

The installer downloads the matching x86_64 Linux archive and checksum, verifies the archive before
atomically installing `dorf` to `~/.local/bin`, and prints a `PATH` handoff when needed. It does not
run setup. A standalone install prints the next-step `dorf setup` guidance; `dorf update` uses the
same verified installer while omitting that fresh-install hint. On a deployment host with Dorf's
managed API and worker already installed, update also hands those services to the new binary for
reconciliation and restart; a remote CLI-only installation remains service-free. Install an exact
release by using its pinned installer asset:

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

On a remote CLI client, installation ends after `dorf version`; continue at
[Connect one remote CLI Client](#3-connect-one-remote-cli-client). Only the deployment host runs the
convergent setup entry point:

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

After persisting deployment configuration, setup shows and reconciles the supported managed API and
worker pair. This happens even when the operator selects no Sandbox provider yet, so the control
plane is ready while Job admission waits for a verified Profile. Use `--yes` to approve the shown
service plan in automation. The public HTTPS ingress remains an independent operator responsibility;
the [Remote Control API](control-api.md#deployment-services) owns the exact service boundary.

When supported Ubuntu 24.04 host changes are needed, setup previews and applies only those exact
changes after approval. `--yes` approves the same host and Cloudflare plans for automation. Setup
reuses one unambiguous ready AI connection when it already exists; otherwise automation must name
one with `--ai-connection` or explicitly select its authentication mode. Sandbox provider choices
remain explicit. `dorf setup --yes` alone prepares the common durable foundation and managed
services; it does not silently select a Sandbox provider.

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

## 3. Connect one remote CLI Client

The deployment host owns setup, Profiles, provider and Harness credentials, PostgreSQL, and the
managed worker. A remote client machine needs only the Dorf CLI, an operator-provided HTTPS
Deployment URL, and one short-lived Enrollment; it does not run `dorf setup`.

The control API URL must be an operator-owned HTTPS origin backed by the managed API's private
listener. The operator must give it a different origin from the Provider Gateway: that separate
`/v1` service provides model access to Sandboxes, not Dorf client operations. Dorf installs and
supervises the private API and worker, but does not provision or infer public ingress. Verify the
host pair before Enrollment:

```bash
dorf service status
```

See the [Remote Control API](control-api.md#deployment-services) for the exact service boundary and
host lifecycle commands.

On the deployment host, create a one-use Enrollment:

```bash
dorf client enroll
```

Transfer the printed code to the intended client through a private channel. On that client, connect
to the Deployment and paste the code when prompted:

```bash
dorf connect https://control.example.com
dorf auth status
```

Use `dorf auth status --output json` for a stable non-interactive identity receipt. The Deployment's
public discovery links its embedded OpenAPI 3.1 document; direct HTTP callers should use that
document and its published Problem catalog rather than infer schemas from CLI prose.

For non-interactive enrollment, put only the code in a protected file and pass
`--enrollment-file PATH`, or use `--enrollment-file -` to read it from standard input. The CLI keeps
one normalized Deployment URL and its client-generated credential in a dedicated owner-only file;
there are no named contexts or context switching.

Save the complete prompt in `goal.txt`, then use the same CLI to admit and operate a direct Job over
HTTPS:

```bash
dorf run --goal-file goal.txt --model MODEL --reasoning high
dorf job list
dorf job list --limit 25 --output json
dorf job inspect JOB_ID
dorf job watch JOB_ID
dorf job watch --output jsonl JOB_ID
dorf job message --input-file follow-up.txt JOB_ID
dorf job message --intent steer --input-file correction.txt JOB_ID
dorf job message inspect JOB_ID MESSAGE_ID
dorf job retry JOB_ID
dorf job evidence JOB_ID
dorf sandbox file get SANDBOX_ID PATH --output DESTINATION
dorf job cleanup JOB_ID
```

To delegate one of the two built-in workflows instead, save the complete coding goal or
investigation brief in a file and use its typed admission command:

```bash
dorf workflow run coding \
  --goal-file goal.txt \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --revision FULL_COMMIT_OID \
  --base main \
  --model MODEL \
  --reasoning high

dorf workflow run codebase-investigation \
  --brief-file brief.txt \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --revision FULL_COMMIT_OID \
  --model MODEL \
  --reasoning high
```

Remote coding uses the deployment's GitHub integration; its request carries no integration
credential. Remote investigation accepts only a credential-free HTTPS repository URL and exact
Revision. `--local-repo` remains a deployment-host-only input that creates a retained Git bundle and
is never sent through the remote API. Both workflow Jobs use the same remote inspect, watch,
Message, retry, file, Evidence, and cleanup commands shown above. Investigation remains open and
idle after settled work until the client requests cleanup. Coding requests cleanup once it observes
a terminal GitHub Outcome, so retrieve any needed Sandbox file before that external decision;
retained Evidence remains readable after cleanup.

`job inspect` reports the initial Message ID and exact Sandbox IDs. Follow may queue before current
work settles; steer targets only the exact active Turn and never becomes a Follow. `job watch`
reconnects from the canonical snapshot, and Ctrl-C stops only the view. Retry is accepted only for
eligible failed execution. Evidence is verified metadata; Sandbox file retrieval returns exact
bytes and must happen before cleanup, which closes Message admission and file reads.

Use `--output json` on Job, Message, retry, and Evidence operations and `--output jsonl` on watch for
stable machine output. The ordinary mutation flow creates retry identity internally and retries the
exact request once after a retryable transport or HTTP server failure; a human does not need to
configure a key. A direct Job remains open and idle after a successful Turn until the caller
requests cleanup.

The deployment operator can inspect the host-owned Client inventory and revoke exactly one Client at
any time using the Client ID reported by `dorf connect` or `dorf auth status`:

```bash
dorf client list
dorf client show CLIENT_ID
dorf client revoke CLIENT_ID
```

All three commands accept `--output json` before the Client ID where applicable. Revocation is
idempotent and makes subsequent authenticated requests from that Client fail without changing other
Clients or Jobs. Client administration is deliberately not a remote API.

## 4. Run a direct Job on the deployment host

On a deployment host without a saved remote Client connection, use the local CLI when you want
controlled agent execution without delegating result meaning or completion policy to a native
workflow. Save the complete prompt in `goal.txt`, then admit it:

```bash
dorf run \
  --goal-file goal.txt \
  --model MODEL \
  --reasoning high

dorf inspect --follow JOB_ID
```

The managed worker claims the Job; do not start a competing foreground worker in the ordinary
deployment flow.

For a human invocation, `--key` is optional: Dorf generates and prints an admission key before
accepting the Job; reuse that key if the command is interrupted. Automation or deliberate replay
should pass a stable `--key` explicitly so the same complete request can be replayed safely.

The verified deployment-default Sandbox profile and AI connection are used unless explicitly
selected. After a successful Turn, the Job remains open and idle so the caller can continue the same
Harness Thread, retrieve an exact workspace file, or request cleanup:

```bash
dorf message --job JOB_ID --id follow-1 --input-file follow-up.txt
dorf sandbox file get SANDBOX_ID PATH --output DESTINATION
dorf cleanup JOB_ID
```

Follow-up Messages may be queued while earlier work is active; Dorf delivers them FIFO as distinct
Turns on the retained Thread. Use `--intent steer` only to target the exact active Turn. Steer has
priority over queued follows, never falls back to a new Turn, and fails honestly if that Turn has
already become terminal. The CLI owns the raw prompt and the meaning of any resulting prose or files;
Dorf owns durable delivery, recovery, the exact
Job-owned Sandbox, and execution of explicit cleanup. No workflow identity, Git repository, or
GitHub integration is required.

## 5. Run a coding Job on the deployment host

This command stays host-local only when the deployment host has no saved remote Client connection;
a connected CLI sends the same typed request to its configured Deployment.

The selected profile owns the Harness. Omit `--profile` to use the verified deployment default.
Create and verify a separate Pi profile when that Job should use Pi; both may reference the same
exact credential-free image.

Save the complete goal in `goal.txt`, then admit it with stable authority:

```bash
dorf workflow run coding \
  --key my-change-v1 \
  --goal-file goal.txt \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --revision FULL_COMMIT_OID \
  --branch dorf/my-change-v1 \
  --base main \
  --model MODEL \
  --reasoning high

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

The managed worker recovers after process loss; use the [service diagnostics](support.md) when an
operator action is needed. Use `dorf message` for later input and `--intent steer` to target active
work. The coding workflow observes the exact pull request for acceptance or
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
