# Agent guide

Use this runbook when a human delegates Dorf installation or operation to an agent. First determine
whether this machine is the deployment host or a remote CLI client; those roles have different
authority. [Getting started](getting-started.md) owns procedures and
[Support and diagnostics](support.md) owns platform limits and fault attribution.

## Role boundary

The **deployment-host agent** may install Dorf, run setup, use Compose directly for advanced
operations, manage Profiles and optional integrations, operate diagnostics, and coordinate storage,
provider, Harness, and operator-owned ingress boundaries. It must pause for every password, secret,
browser authorization, paid service, administrator helper, or consequential infrastructure choice.

The **remote-client agent** installs and uses only the Dorf CLI. It may connect, check its own
authentication, admit a direct Job or either documented built-in workflow, inspect and watch the
Job, list bounded Job references, send or inspect Messages, request eligible retry, read an exact
Sandbox file, inspect Evidence, and request cleanup. It must not run deployment-host commands such
as `dorf setup`, `dorf serve`, `dorf worker`, `dorf client`, database or migration commands,
`dorf provider`, `dorf profile`, or GitHub integration setup, and it must not operate the deployment
host's Compose project. It must not use SSH or local-only commands to work around a missing remote
capability. The deployment host owns all of those operations.

## Common installation protocol

1. Confirm this is a supported client or deployment platform and read the two authorities linked
   above. Do not infer requirements from `docs/research/` or `docs/history/`.
2. Use the immutable-release installer or attested manual fallback from Getting started. Use the
   contributor build only when the human explicitly requests a source build. Follow any printed
   `PATH` handoff and prove `dorf version`; the installer itself must not run setup.
3. Never read, print, or copy a credential or one-time Enrollment into chat. Let the human enter it
   at the CLI prompt, or consume an explicitly supplied protected file or standard input.

## Deployment-host protocol

Follow the complete setup and readiness procedure in Getting started. Let `dorf setup` select and
prepare the approved Sandbox and Harness path and write the protected `.env` for the installed
static Compose manifests. Setup automatically applies only that exact project as configuration
becomes ready, waits for health, and resumes the guided flow; do not add a separate start command,
permission prompt, manual Compose handoff, or unnecessary second setup run. Its host-prerequisite
checks do not install or mutate them. If it offers an administrator helper, follow the exact identity
and privilege handoff in
[Getting started](getting-started.md#1-install-the-application-initialize-a-deployment-host), pause
for explicit human authorization, then rerun setup. Honor its Docker-authority warning even when no
escalation prompt appears.

### Coordinate remote Incus setup

Use this path only when the deployment host cannot run KVM and a separate owner-controlled x86_64
Linux workstation can. The [remote Incus workstation
procedure](getting-started.md#prepare-a-remote-incus-workstation) owns the exact requirements,
commands, Tailscale policy, and verification. [Support and diagnostics](support.md) owns supported
platforms and network limits.

Coordinate the two hosts in this order:

1. Identify the deployment host and the Incus workstation. Confirm both hosts meet the linked
   requirements before changing either host.
2. Install the same Dorf release on both hosts. Do not copy a helper from another release.
3. Ask the human to approve and apply the narrow tailnet identity and grant. Review other matching
   Tailscale rules because grants are additive.
4. On the workstation, let the human inspect and authorize the installed preparation helper. Run
   only that exact helper after authorization. Leave missing host prerequisites and any other
   firewall or tailnet change to the human.
5. Create the short-lived Incus offer on the workstation. Transfer it through the human-approved
   private channel, never through chat or an agent transcript.
6. On the deployment host, run the linked `dorf setup` flow before the offer expires. Return to the
   workstation to verify the retained client restriction.
7. Prove the selected Profile with the setup result and the documented `dorf doctor` check. Report
   the two host roles, the installed version, the verified Profile, and the retained client
   fingerprint. Do not report the offer or credentials.

Keep the supported topology intact. Do not substitute another transport or broaden its authority.
If the hardware, operating system, network policy, or provider differs from the supported path,
stop and report the boundary from Support. Do not adapt the procedure by inference. The
non-normative [private provider attachment playbook](research/private-provider-attachment.md) is the
starting point for a future provider design, not permission to change a deployment.

Do not invent another Dorf lifecycle wrapper, edit the protected generated `.env`, or start separate
foreground Dorf processes. Use Compose directly only for the advanced observation and process
operations documented in Getting started.

On reruns, follow [Getting started](getting-started.md#1-install-the-application-initialize-a-deployment-host).
Do not pass Sandbox setup flags unless the operator asked to change Sandbox configuration.

Cloudflare has no shell helper. Follow the domain and editable public-hostname flow in
[Getting started](getting-started.md#1-install-the-application-initialize-a-deployment-host), pausing
for the human browser authorization or DNS-replacement choice it requests. Never infer replacement
authority. Setup reuses its persisted exact pair on rerun. Prove the project through that procedure
and [Support](support.md), then run the documented Profile, Provider Gateway, and `dorf doctor`
checks with the exact selected names.

Optional integrations remain host concerns. Pause while the human completes browser authorization,
and return a short-lived code only to its waiting command. Let runtime composition supply integration
authority instead of putting credentials or integration settings in a Job request.

Keep the Control API and Provider Gateway origins distinct even when guided setup routes both through
one Tunnel. Custom ingress remains operator-owned. Do not infer service readiness from a process or
terminal merely remaining open; use Compose state and setup's factual readiness result.

Setup enrolls an ordinary deployment-host Client for Job commands. Use the documented `dorf job`
commands on the host. Never bypass an unavailable API by reading or mutating Job rows directly.

## Remote-client protocol

Receive the exact HTTPS Deployment origin and Enrollment through the human-approved private handoff,
then follow the remote-client procedure in Getting started. One successful `dorf connect` saves one
Deployment; there are no contexts to choose or switch. Prove the binding with `dorf auth status`.
Use `--output json` for a non-interactive receipt, and use `dorf job list` with its opaque returned
cursor when the human has not supplied a Job ID.

Put the complete goal or Message in a file and use the exact remote commands in Getting started.
Mutation retry identity is automatic; do not invent or ask the human to manage a key in the ordinary
flow. Follow may queue, while steer requires the exact active Turn and must not be resent as Follow
after `steer_unavailable`. Retrieve needed Sandbox files before cleanup. An open Job may be idle after
a successful Turn; do not create a replacement merely because it has no active execution. For a
remote investigation, run the report retrieval command printed by `dorf job inspect` before its
printed cleanup command. Remote investigation accepts only its documented credential-free HTTPS
source at an exact Revision.
Use the Deployment-published OpenAPI and Problem catalog for direct code-mode HTTP. Do not invent an
MCP or SDK layer.

## Safety and handback

Never expose Enrollment codes, Client credentials or configuration, environment dumps, Gateway
state, provider credentials, or unrequested Harness transcripts. A caller-requested Message result
may be returned narrowly; do not dump complete inspection, watch, or Message output. Never delete
provider resources or PostgreSQL rows directly. Stop and report the exact failed check when repair
needs authority the human has not granted.

Report only the installed version, the role operated, readiness actually observed, Job identity and
current state, caller-requested output, and cleanup state. A deployment-host handback may also name
the selected Profile and AI connection. Do not claim installation, work, authentication, or cleanup
succeeded merely because a command was started; use the factual CLI result for that role.
