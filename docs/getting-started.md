# Getting started

Check [Support and diagnostics](support.md) before installing Dorf.

## 1. Install the application; initialize a deployment host

Beginning with the first release after `v0.3.0`, install the latest immutable Dorf release:

```bash
curl -fsSL https://github.com/aphronio/dorf/releases/latest/download/install.sh | sh
```

The installer downloads and verifies the complete matching x86_64 Linux archive, then installs
`dorf`, `dorf-compose.yaml`, `dorf-compose-incus.yaml`, and `dorf-incus-remote.sh` together in
`~/.local/bin`. It replaces each file atomically in that directory. It prints a
`PATH` handoff when needed and does not run setup or Docker Compose. `dorf update` replaces the same
four files without changing a running deployment. Install an exact release with its pinned
installer asset:

```bash
RELEASE_TAG=vX.Y.Z
curl -fsSL "https://github.com/aphronio/dorf/releases/download/$RELEASE_TAG/install.sh" | sh
```

To verify GitHub's signed release attestation and install manually, install the complete release
set beside one another:

```bash
RELEASE_TAG=vX.Y.Z
release_dir="$(mktemp -d)"
gh release verify "$RELEASE_TAG" --repo aphronio/dorf
gh release download "$RELEASE_TAG" --repo aphronio/dorf --dir "$release_dir" \
  --pattern "dorf_${RELEASE_TAG#v}_linux_x86_64.tar.gz" \
  --pattern "dorf_${RELEASE_TAG#v}_checksums.txt"
(cd "$release_dir" && sha256sum --check "dorf_${RELEASE_TAG#v}_checksums.txt")
tar -xzf "$release_dir/dorf_${RELEASE_TAG#v}_linux_x86_64.tar.gz" -C "$release_dir"
mkdir -p "$HOME/.local/bin"
install -m 0755 "$release_dir/dorf" "$HOME/.local/bin/dorf"
install -m 0644 "$release_dir/dorf-compose.yaml" "$HOME/.local/bin/dorf-compose.yaml"
install -m 0644 "$release_dir/dorf-compose-incus.yaml" \
  "$HOME/.local/bin/dorf-compose-incus.yaml"
install -m 0755 "$release_dir/bootstrap/incus-remote.sh" \
  "$HOME/.local/bin/dorf-incus-remote.sh"
dorf version
```

Contributors building from source should instead use the repository-managed toolchain in
[CONTRIBUTING.md](../CONTRIBUTING.md).

On a remote CLI client, installation ends after `dorf version`; continue at
[Connect one remote CLI Client](#3-connect-one-remote-cli-client). Only the deployment host runs the
resumable setup entry point:

```bash
dorf setup
```

Setup first checks that Docker Engine and its Compose plugin are usable. When they are unavailable,
setup materializes the version-matched `docker.sh` helper, which prepares both Engine and Compose
on its stated clean Ubuntu 24.04 noble amd64 target. It prints the command an administrator may
inspect and run and links the upstream
[Docker Engine](https://docs.docker.com/engine/install/) and
[Compose plugin](https://docs.docker.com/compose/install/linux/) authorities. Dorf never runs the
helper, invokes `sudo`, elevates, or changes identity. After the invoking operator prepares Docker,
rerun `dorf setup`. Docker-daemon access may be root-equivalent authority, but Dorf does not acquire
it. On another host, follow the linked upstream procedure.

Setup writes a protected `.env` under
`${XDG_DATA_HOME:-$HOME/.local/share}/dorf-compose`. The installed static manifests remain beside
the binary; `.env` points Docker Compose at the base manifest and, for a local Incus endpoint, its
static overlay. As configuration becomes sufficient and whenever those protected inputs change,
setup automatically applies that exact installed project and waits for it to become healthy before
continuing the same guided flow. Its one-shot `migrate` service must complete successfully before
the worker and control API start. Calling `dorf setup` is the deployment intent; there is no extra
Compose permission prompt, manual start handoff, separate `dorf start`, or setup-start-setup loop.
That invoking identity may be root or non-root. Guided Cloudflare setup also completes Dorf's two
public origins; deployments using another ingress keep that responsibility with their operator.

The official release configuration selects the exact
`ghcr.io/aphronio/dorf:MAJOR.MINOR.PATCH` image with `pull_policy: always`. Dorf does not render
Compose YAML, install Docker, inspect arbitrary Docker resources, or provide a general lifecycle
wrapper. After `dorf update`, one `dorf setup` run applies the updated installed manifests and
continues through factual readiness. While Compose runs, setup names the image check, migration, and
service-health work rather than reporting a generic service start. On a rerun without Sandbox setup
flags, Dorf resumes with the retained default profile's provider, name, and Harness instead of
showing the provider or Harness selectors. Its result lists every retained Sandbox profile and
reports a configured E2B credential separately from an E2B profile. To add or configure a different
named profile after the deployment is ready, use `dorf profile add`; do not rerun the whole setup
flow for ordinary profile management.

For advanced observation and process operations, use Docker Compose itself from the generated
project directory:

```bash
docker compose ps
docker compose restart worker control-api
docker compose logs --tail=200 worker control-api
```

Do not edit the generated `.env`; rerun setup to change and apply its source facts.

Setup offers prepared local Incus, remote Incus over Tailscale, cloud E2B, any combination, or none.
A selected local Incus endpoint must already be usable. For the default
`unix:///var/lib/incus/unix.socket` authority only, setup can materialize the version-matched
`incus.sh` administrator helper, print its exact command and the upstream manual path, and exit. A
custom Unix socket receives an exact repair-or-select handoff instead of a host recipe. Dorf does
not install Incus or QEMU, enable a service, change group membership, initialize the daemon, or
mutate a host network. The manual authority is the upstream [Incus installation
guide](https://linuxcontainers.org/incus/docs/main/installing/). The invoking operator owns every
administrator action and login handoff; Dorf never runs a helper, elevates, or changes identity.
Rerun setup afterward as that same operator identity.

A Dorf Deployment configures at most one Incus endpoint, while each Incus Profile owns its
restricted project, pool, network, exact image, disk contract, and guest-reachable Provider Gateway
URL. Guided local setup may create that route from one unambiguous prepared bridge observation; the
[Provider Gateway authority](project/provider-gateway.md) owns the exact persistence and
no-runtime-inference rule. A remote Incus Profile requires the stable HTTPS Gateway prepared later
in this procedure. There is no migration or adoption path for an earlier Profile shape; create and
verify a current Profile.

### Prepare a remote Incus workstation

Use this path when the Dorf deployment host cannot run KVM and an owner-controlled x86_64 Linux
workstation can. Install the same Dorf release on both hosts. The workstation needs Incus 7.3 or
newer, KVM, UFW, and Tailscale. The deployment host needs Tailscale but does not need Incus or KVM.

Give the deployment host only the Tailscale identity `tag:dorf-controller`. Keep the workstation as
a user-owned device. Add one host alias and one grant to the existing tailnet policy; do not replace
unrelated policy:

```json
{
  "tagOwners": {
    "tag:dorf-controller": ["OWNER@example.com"]
  },
  "hosts": {
    "dorf-incus-workstation": "WORKSTATION_TAILSCALE_IPV4"
  },
  "grants": [
    {
      "src": ["tag:dorf-controller"],
      "dst": ["dorf-incus-workstation"],
      "ip": ["tcp:8443"]
    }
  ]
}
```

Defining a tag does not assign it. In the Tailscale admin console, apply `tag:dorf-controller` to
the deployment host and confirm that the Machines page shows that tag before continuing. A tagged
authentication key is the automation alternative described by Tailscale's tag rules.

Tailscale grants are additive. Review every other matching grant or ACL so the tagged controller
cannot reach another workstation port. Use the current [Tailscale grants
syntax](https://tailscale.com/docs/reference/syntax/grants) and [tag
rules](https://tailscale.com/docs/features/tags) as the policy authority. Do not enable Tailscale
SSH, subnet routing, an exit node, Serve, or Funnel for this connection. On the controller, reject
incoming tailnet connections and ignore tailnet DNS and subnet routes:

```bash
sudo tailscale set --shields-up --accept-dns=false --accept-routes=false
```

On the workstation, inspect the installed helper. Then prepare the fixed `dorf-remote` project,
`dorfbr0` bridge, `dorf-egress` ACL, kernel module declaration, and UFW policy:

```bash
sudo -- "$HOME/.local/bin/dorf-incus-remote.sh" prepare \
  --acknowledge-kernel-module-impact \
  --acknowledge-firewall-impact
```

The helper preserves unrelated Incus projects, networks, and instances. It gives Job VMs public
IPv4 egress through NAT, disables IPv6, isolates VM peers, and blocks the workstation, LAN,
link-local, and tailnet ranges. It binds no listener during `prepare`.

After the tailnet grant is active, create one protected 15-minute offer. The `offer` action binds
native Incus HTTPS to the workstation's exact Tailscale IPv4 on TCP 8443 and prints only the offer:

```bash
umask 077
sudo -- "$HOME/.local/bin/dorf-incus-remote.sh" offer \
  --acknowledge-remote-incus-exposure > incus-offer
```

Transfer `incus-offer` to the deployment host through a private channel. Run setup before it
expires:

```bash
dorf setup \
  --sandbox-provider incus \
  --incus-trust-offer-file "$HOME/incus-offer"
```

Setup pins the server certificate, creates a fresh client key, redeems the offer, proves that the
client is restricted to `dorf-remote`, and removes its pending record after retention. It then uses
the fixed `default` pool and `dorfbr0` network, installs the official image, creates a Profile with
the stable HTTPS Provider Gateway URL, and verifies a disposable VM. Delete both offer-file copies
after setup succeeds. Use `--incus-trust-offer-file -` to stream an offer without retaining a
controller copy.

Setup prints the retained client fingerprint. On the workstation, verify its exact restriction:

```bash
sudo -- "$HOME/.local/bin/dorf-incus-remote.sh" inspect \
  --fingerprint CLIENT_CERTIFICATE_SHA256
```

The Incus HTTPS listener stays on the Tailscale address after enrollment. The restricted client can
create and operate Dorf VMs but cannot edit the administrator-owned bridge or ACL. To cut off that
controller, use the helper's explicit `revoke` action with the printed fingerprint and its required
acknowledgement.

Selecting a provider continues through Harness choice, ChatGPT-subscription or OpenAI-API
authentication, provider inputs, profile creation, functional verification, and default selection.
E2B uses Dorf's exact public Standard template build unless `--e2b-template` selects a custom exact
build, and needs one stable HTTPS `/v1` Gateway route. The [Provider Gateway
authority](project/provider-gateway.md) owns retained-candidate replay, protected Compose-input
publication, live verification, and default-commit semantics. Automation can name a candidate with
`--ai-connection` or explicitly select its authentication mode. Sandbox provider choices remain
explicit; `dorf setup --yes` does not silently select one.

After initial readiness, `dorf profile add` is the guided profile-management path. It uses provider
access and the model Gateway already configured by setup, selects the official provider artifact,
creates or resumes the named profile, performs the mandatory functional verification and cleanup,
and preserves an existing default unless the operator selects the new profile or passes
`--set-default`; the first verified profile becomes the default. With no name, it derives
`incus-HARNESS`, `local-HARNESS`, or `cloud-HARNESS` from the configured provider and Harness.
Non-interactive use names the provider explicitly:

```bash
dorf profile add --sandbox-provider e2b --harness codex
```

Use `profile create` to adopt an exact existing provider artifact, `profile install` for an exact
official Incus release, `profile update` for an existing definition, and `profile verify` when only
verification must be rerun. Those precise commands remain the automation and custom-artifact path.

The Provider Gateway joins the static Compose project when an AI connection is configured. Setup
publishes that profile into the protected `.env`, reapplies the project, and continues to verify and
finalize the retained candidate in the same run. It can verify an existing Sandbox-reachable route
or guide the named Cloudflare Tunnel owned by the [Provider Gateway
authority](project/provider-gateway.md).

The guided path first asks for a Dorf domain, for example `dorf.run`. It leaves that apex untouched
and proposes two editable direct child hostnames:

```text
api.dorf.run     Control API
models.dorf.run  Model Gateway
```

The inputs are hostnames, not URLs; setup fixes HTTPS for both and `/v1` for the Model Gateway. One
named outbound-only Tunnel routes the exact selected pair. A rerun reuses that persisted pair
rather than deriving new names. Fresh unused names proceed without another confirmation. If either
name resolves through unrelated DNS, setup requires an explicit replacement choice before changing
it.

Automation supplies `--cloudflare-domain DOMAIN`; optional
`--cloudflare-control-hostname HOST` and `--cloudflare-model-hostname HOST` replace the two suggested
names. Pair `--replace-cloudflare-dns` with that selection only when replacement is intended. Setup
verifies both public routes and prints the Control API origin for `dorf connect`. This remains an
unprivileged browser and DNS flow, not another shell helper. Advanced `--gateway-url` retains an
existing exact Provider Gateway route and leaves custom Control API ingress to the operator.
It is not mixed with retained Dorf-owned Tunnel state; remove that managed ingress before switching
to custom origins.

The separate `profile` and `provider` commands remain available for custom artifacts and advanced
operations. Their exact-artifact, credential, and route boundaries are described by the
[release process](releasing.md) and [Provider Gateway](project/provider-gateway.md).

## 2. Set up the optional GitHub integration

Skip this section when a client or workflow needs only credential-free access to a public Git
repository. A GitHub App is required for authenticated GitHub API or repository operations.

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
managed Compose project. A remote client machine needs only the Dorf CLI, the HTTPS Deployment
origin printed by guided setup or supplied by the operator, and one short-lived Enrollment; it does
not run `dorf setup`.

The Control API service listens on container port `8745`. Guided Cloudflare reaches it over the
Compose ingress network and prints that origin; custom operator-owned HTTPS ingress reaches the
published host port `8745`. The Provider Gateway uses the separate model origin prepared by setup,
or the exact custom route selected with `--gateway-url`; it provides model access to Sandboxes, not
Dorf client operations. Before Enrollment, complete the continuous setup flow in
[the deployment-host procedure](#1-install-the-application-initialize-a-deployment-host). The
[Remote Control API](control-api.md#deployment-services) owns the exact capability and service
boundary.

On the deployment host, create a one-use Enrollment:

```bash
dorf client enroll
```

Transfer the printed code to the intended client through a private channel. On that client, connect
to the Deployment and paste the code when prompted:

```bash
dorf connect https://api.dorf.run
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
dorf run --goal-file goal.txt --ai-connection AI_CONNECTION --model MODEL --reasoning high
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
dorf job abandon JOB_ID
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
  --ai-connection AI_CONNECTION \
  --model MODEL \
  --reasoning high

dorf workflow run codebase-investigation \
  --brief-file brief.txt \
  --repo https://github.com/OWNER/REPOSITORY.git \
  --revision FULL_COMMIT_OID \
  --ai-connection AI_CONNECTION \
  --model MODEL \
  --reasoning high
```

Remote coding uses the deployment's GitHub integration; its request carries no integration
credential. Investigation accepts only a credential-free HTTPS repository URL and exact Revision.
Both workflow Jobs use the same inspect, watch, Message, retry, file, Evidence, and cleanup commands
shown above.
Investigation remains open and idle after settled work until the client requests cleanup. Coding
requests cleanup once it observes a terminal GitHub Outcome, so retrieve any needed Sandbox file
before that external decision;
retained Evidence remains readable after cleanup.

`job inspect` reports the Job ID, initial Message ID, and exact Sandbox IDs. For an investigation,
it also prints the exact report retrieval command followed by the cleanup command. Follow may queue
before current work settles. Steer targets only the exact active Turn and never becomes a Follow.
`job watch` reconnects from the canonical snapshot, and Ctrl-C stops only the view. Retry is
accepted only for eligible failed execution. Evidence is verified metadata. Sandbox file retrieval
returns exact bytes and must happen before cleanup, which closes Message admission and file reads.

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

Setup enrolls an ordinary deployment-host Client after the Compose API becomes ready. The Client
uses the fixed loopback origin and the same API as a remote CLI. A saved remote `client.json` takes
precedence. Save the complete prompt in `goal.txt`, then admit it:

```bash
dorf run \
  --goal-file goal.txt \
  --ai-connection AI_CONNECTION \
  --model MODEL \
  --reasoning high

dorf job watch JOB_ID
```

The Compose-managed worker claims the Job; do not start a competing foreground worker in the ordinary
deployment flow.

For a human invocation, `--key` is optional. Dorf generates a key and retries one ambiguous API
failure with the same request. Automation or deliberate replay should pass a stable `--key`.

The verified deployment-default Sandbox profile and AI connection are used unless explicitly
selected. After a successful Turn, the Job remains open and idle so the caller can continue the same
Harness Thread, retrieve an exact workspace file, or request cleanup:

```bash
dorf job message --key follow-1 --input-file follow-up.txt JOB_ID
dorf sandbox file get SANDBOX_ID PATH --output DESTINATION
dorf job cleanup JOB_ID
```

Follow-up Messages may be queued while earlier work is active; Dorf delivers them FIFO as distinct
Turns on the retained Thread. Use `--intent steer` only to target the exact active Turn. Steer has
priority over queued follows, never falls back to a new Turn, and fails honestly if that Turn has
already become terminal. The CLI owns the raw prompt and the meaning of any resulting prose or files;
Dorf owns durable delivery, recovery, the exact
Job-owned Sandbox, and execution of explicit cleanup. No workflow identity, Git repository, or
GitHub integration is required.

## 5. Run a coding Job on the deployment host

This command uses the same authenticated control API on the deployment host and a remote client.

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
  --ai-connection AI_CONNECTION \
  --model MODEL \
  --reasoning high

dorf job inspect JOB_ID
```

Admission derives the exact GitHub owner/repository from `--repo`. The coding runtime composed for
the selected profile discovers the deployment-default App installation before admitting a new Job;
the Job request carries no integration or permission settings. The caller supplies the exact starting
`--revision` and base. Retrying an existing key reuses its retained installation so recovery does not
depend on GitHub availability; the complete caller input must still match.

To follow the same durable facts without repeatedly invoking inspection, use:

```bash
dorf job watch JOB_ID
```

The watcher reads canonical Job snapshots. `Ctrl-C` stops only the view, not the Job.

The Compose-managed worker recovers after process loss; use [Support](support.md) when an operator
action is needed. Use `dorf job message` for later input and `--intent steer` to target active
work. The coding workflow observes the exact pull request for acceptance or
rejection and requests cleanup after its terminal policy is satisfied. To stop without a GitHub
decision:

```bash
dorf job abandon JOB_ID
dorf job inspect JOB_ID
```

If `dorf job inspect JOB_ID` reports that the workflow stopped, repair the displayed cause and run
`dorf job retry JOB_ID`. This schedules exactly one more bounded attempt on the same Absurd task and
retains its checkpoints. The receipt reports scheduling identities but does not claim that a worker
has resumed it yet; use `dorf job inspect JOB_ID` to observe current work and progress.

Cleanup remains separately observable. `dorf job cleanup JOB_ID` is an explicit client request to
release the Job's resources; Core reconciles that request or retries an incomplete cleanup, then
inspection reports the resulting facts.
