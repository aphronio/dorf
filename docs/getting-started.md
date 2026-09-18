# Getting started

Check the [supported-platform matrix and host prerequisites](support.md) before installing Dorf.
This guide provides commands for the supported configurations; it does not define platform
support.

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

When upgrading from the Job API to the Session API, stop both old processes with
`docker compose stop worker control-api` before running updated `dorf setup`. Setup applies the
migration and starts the updated services. Update clients to the Session API in the same deployment;
the old routes and CLI names are removed. Existing IDs, input, queue work, native conversations,
and resource ownership are retained. Do not run old binaries against the renamed schema.
See [D146](project/decisions/D146-name-the-execution-context-session.md).

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

New Dorf Incus VMs receive 4 vCPUs and 8 GiB RAM. Dorf does not resize existing VMs when it
reconnects to them. Guided setup provisions a 40 GiB root disk.

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

The helper preserves unrelated Incus projects, networks, and instances. It gives Session VMs public
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
official Incus release, and `profile update` to stage a replacement definition. `profile verify`
checks that candidate and promotes it only after the probe and its Sandbox cleanup succeed. The
previous active revision and default selection remain usable during a replacement's verification;
running Sessions keep their original revision. No Session cleanup is required to update a profile.
`profile show` reports the candidate definition and `active_revision`; `profile list` reports the
active definition where one exists. Rerunning `profile verify` on an unchanged active revision
refreshes its proof and temporarily fences new admissions. These commands remain the automation
and custom-artifact path.

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

## 3. Connect one remote CLI Client

The deployment host owns setup, Profiles, provider and Harness credentials, PostgreSQL, and the
managed Compose project. A remote client machine needs only the Dorf CLI, the HTTPS Deployment
origin printed by guided setup or supplied by the operator, and one short-lived Enrollment; it does
not run `dorf setup`.

Remote Clients can discover Sandbox profiles for Session selection; profile administration stays on
the deployment host.

The Control API service listens on container port `8745`. Guided Cloudflare reaches it over the
Compose ingress network and prints that origin; custom operator-owned HTTPS ingress reaches the
published host port `8745`. The Provider Gateway uses the separate model origin prepared by setup,
or the exact custom route selected with `--gateway-url`; it provides model access to Sandboxes, not
Dorf client operations. Before Enrollment, complete the continuous setup flow in
[the deployment-host procedure](#1-install-the-application-initialize-a-deployment-host). The
[Remote Control API](control-api.md#deployment-services) explains the operator-facing service
boundary; the checked-in [`deploy/compose.yaml`](../deploy/compose.yaml) owns the exact managed
service and network inventory.

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

List the deployment's Sandbox profiles before choosing one:

```bash
dorf profile list
dorf profile list --output json
```

The list includes the provider, Harness, default status, and recorded verification status. It does
not run a live readiness check. Select a verified name with `--profile NAME`, or omit `--profile`
to use the deployment default. An unknown name returns `profile_not_found`; list again to choose a
current name. The same listing commands work on the deployment host through its enrolled Client.
`profile show NAME` remains a deployment-host command for inspecting the full configuration.

Save the complete prompt in `goal.txt`, then use the same CLI to admit a direct Session and perform the
operations needed for this walkthrough:

```bash
dorf run --input-file message.txt --ai-connection AI_CONNECTION --reasoning high
dorf run --attach diagram.png --attach notes.txt --ai-connection AI_CONNECTION
dorf session list
dorf session list --limit 25 --output json
dorf session inspect SESSION_ID
dorf session watch SESSION_ID
dorf session watch --output jsonl SESSION_ID
dorf session event --input-file follow-up.txt SESSION_ID
dorf session event --attach screenshot.png SESSION_ID
dorf session turns SESSION_ID
dorf session history SESSION_ID
dorf session cancel SESSION_ID
dorf session retry SESSION_ID
dorf sandbox file get SANDBOX_ID PATH --output DESTINATION
dorf session abandon SESSION_ID
dorf session cleanup SESSION_ID
```

Use `--client-reference REFERENCE` with `dorf run` to
attach your thread or task reference. `dorf session list` and `dorf session inspect` show the creating
Client and reference. An older Session shows an unknown creator. Use that information when choosing
cleanup targets; attribution does not request cleanup or define a retention policy.
Client configuration may set `client_reference` as the default for new Sessions; an explicit flag
overrides it.

`run` creates the Session, waits for resource readiness and submits native input once. Its key
identifies Session creation only; rerunning it can submit new input. An unknown input result requires
native inspection before another send. The response includes the native Turn ID. The Harness chooses
whether input joins active work or starts another Turn. No explicit Follow/Steer or offline queue exists.

Use Session watch for resources and native Turns/history for execution. Stopping a watcher leaves
native work running. Retry applies to eligible failed lifecycle work; it never replays native input.
Retrieve files before cleanup. Exact file reads reject traversal, symlinks and directories.

The deployment operator can inspect the host-owned Client inventory and revoke exactly one Client at
any time using the Client ID reported by `dorf connect` or `dorf auth status`:

```bash
dorf client list
dorf client show CLIENT_ID
dorf client revoke CLIENT_ID
```

All three commands accept `--output json` before the Client ID where applicable. Revocation is
idempotent and makes subsequent authenticated requests from that Client fail without changing other
Clients or Sessions. Client administration is deliberately not a remote API.

For an unattended integration, issue a dedicated key on the deployment host:

```bash
dorf client issue-key --name agent0 --credential-file /protected/path/agent0.key --no-expiry
```

The parent directory must already exist and be controlled by the operator. The command creates a
new owner-only file containing only the bearer credential and refuses existing files or symlinks.
It prints the Client ID and public metadata, never the credential. JSON output is available with
`--output json`. Transfer the file through your protected secret-delivery path, then configure the
integration to send its contents as `Authorization: Bearer <credential>` to the Deployment HTTPS
origin. Do not put the credential in shell arguments, logs, or source control.

The explicit `--no-expiry` key remains valid until you revoke its Client ID. To rotate, issue a new
key into a new file, update the integration, verify authentication, then revoke the previous Client.
If database registration fails, the command retains the protected file because the commit outcome
may be uncertain. Inspect the Client inventory and revoke any unwanted Client before retrying with
a new file. A failed file write never registers a Client.

## 4. Run a direct Session on the deployment host

Setup enrolls an ordinary deployment-host Client after the Compose API becomes ready. The Client
uses the fixed loopback origin and the same API as a remote CLI. A saved remote `client.json` takes
precedence. Save the complete prompt in `goal.txt`, then admit it:

```bash
dorf run \
  --input-file message.txt \
  --attach diagram.png \
  --attach requirements.pdf \
  --ai-connection AI_CONNECTION \
  --reasoning high

dorf session watch SESSION_ID
```

The Compose-managed worker claims the Session; do not start a competing foreground worker in the ordinary
deployment flow.

Session creation uses an idempotency key; native input does not. The CLI never automatically retries
an ambiguous native mutation. Use the API directly to separate create, workspace setup and input.
The verified default profile and model connection apply unless selected explicitly.

```bash
dorf session event --client-id input-2 --input-file follow-up.txt SESSION_ID
dorf session event --attach screenshot.png --attach notes.txt SESSION_ID
dorf session turns SESSION_ID
dorf session history SESSION_ID
dorf session cancel SESSION_ID
dorf sandbox file get SANDBOX_ID PATH --output DESTINATION
dorf session cleanup SESSION_ID
```

`--client-id` is attribution, not duplicate suppression. Attachments travel by value; an input file is
optional when attachments exist. Add `--refresh-skills` to request native skill-catalog refresh.
The [API semantics](control-api.md#resources) describe acknowledgement, uncertainty and observation.
Cancel targets the current native Turn once; a new call may target newer work. Do not retry a lost
cancel response. Native history and workspace files follow the supported storage/recovery contract.

## 5. Continue and release a Session

Use `dorf session watch SESSION_ID` to observe current facts. Stopping the watcher leaves work running.
The Compose-managed worker recovers after process loss; use [Support](support.md) when operator
action is needed. `dorf session retry SESSION_ID` schedules one more attempt for eligible failed execution.
`dorf session cleanup SESSION_ID` closes admission and reconciles resource release.

Clients own repository setup, reviews, publication credentials and business outcomes. Dorf retains
Session/resource/lifecycle facts; input and execution remain native. There is no application archive.

### Keep a worker running between turns

New E2B-backed Sessions pause their Sandboxes when idle. Add `--keep-running` to `dorf run` when background work
must continue between turns. The override is saved with the Session and must match on an explicit
admission replay. It does not disable provider timeout limits. Other providers keep their
existing lifecycle until their pause capability is supported.

## Upgrade a retained Codex workspace

For an existing direct Session, first stage a verified immutable Nix closure inside its Sandbox. New
images include `dorf-packages stage VERSION`, which downloads a supported pinned version using the
guest's existing Internet access without changing the active Codex. Its retained store path is
available through `readlink -f /usr/local/share/dorf/packages/generations/VERSION`. Older images
require the one-time Nix bootstrap described by the
[upgrade recipe](../scripts/runtime-upgrade/README.md). Staging must preserve the Sandbox's network
policy. Then request activation with an exact reusable ID:

```bash
dorf upgrade request SESSION --id upgrade-20260915-example --package /nix/store/HASH-codex-VERSION --version VERSION
dorf upgrade show SESSION
```

Replace the placeholders with the real Session ID, full staged store path, and exact package version.
The hold rejects new native input; the client retains unsent work. The worker checkpoints local state, activates the
package, and verifies the retained conversation. Failed verification restores the checkpoint before
resuming. A failed recovery retains the hold and exposes attention; retry the existing failed Session
using its ordinary retry command after addressing the reported cause. Repeating the same upgrade ID
never changes its package or reopens a completed hold.

The Session and logical Sandbox IDs stay stable. E2B rollback can replace the underlying VM; Session
inspection retains both resources. Automatic distribution and fleet rollout are not implemented.
