# Agent guide

Use this runbook when a human delegates Dorf installation or operation to an agent. First determine
whether this machine is the deployment host or a remote CLI client; those roles have different
authority. [Getting started](getting-started.md) owns procedures and
[Support and diagnostics](support.md) owns platform limits and fault attribution.

## Role boundary

The **deployment-host agent** may install Dorf, run setup, manage Profiles and optional integrations,
operate diagnostics, reconcile Dorf's managed Compose project, and coordinate storage,
provider, Harness, and operator-owned ingress boundaries. It must pause for every password, secret,
browser authorization, paid service, administrator helper, or consequential infrastructure choice.

The **remote-client agent** installs and uses only the Dorf CLI. It may connect, check its own
authentication, admit a direct Job or either documented built-in workflow, inspect and watch the
Job, list bounded Job references, send or inspect Messages, request eligible retry, read an exact
Sandbox file, inspect Evidence, and request cleanup. It must not run deployment-host commands such
as `dorf setup`, `dorf serve`, `dorf worker`, `dorf service`, `dorf client`, database or migration
commands, `dorf provider`, `dorf profile`, or GitHub integration setup. It must not use SSH or
local-only commands to work around a missing remote capability. The deployment host owns all of
those operations.

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
prepare the approved Sandbox and Harness path. Setup's host-prerequisite checks are read-only. If it
offers an administrator helper, follow the exact handoff in
[Getting started](getting-started.md#1-install-the-application-initialize-a-deployment-host), pause
for explicit human authorization, then rerun setup. Honor its Docker-authority warning even when no
escalation prompt appears. The same pause-and-rerun rule applies when Compose reconciliation offers
the fixed legacy-systemd retirement helper; do not stop, disable, edit, or remove units directly.

Use setup's local image transport options only when the human explicitly requests the contributor
or disposable-proof path documented in Getting started.

Cloudflare has no shell helper. Keep its existing guided browser, DNS, and named-Tunnel flow, pausing
for the human authorization it requests. Prove the managed Compose project with the
[service diagnostic](support.md), and use only the lifecycle commands and targets documented by the
[deployment-service authority](control-api.md#deployment-services). Run the documented Profile,
Provider Gateway, and `dorf doctor` checks with the exact selected names. Do not directly supervise
managed project processes.

Optional integrations remain host concerns. Pause while the human completes browser authorization,
and return a short-lived code only to its waiting command. Let runtime composition supply integration
authority instead of putting credentials or integration settings in a Job request.

Keep the operator-owned HTTPS ingress distinct from both the Compose-managed private control API and
the Provider Gateway origin. Do not infer service readiness from a process or terminal merely
remaining open; use the factual status result.

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
a successful Turn; do not create a replacement merely because it has no active execution. Remote
investigation accepts only its documented credential-free HTTPS source; never upload, copy, or
reconstruct a host-local retained bundle through the remote boundary. Use the Deployment-published
OpenAPI and Problem catalog for direct code-mode HTTP; do not invent an MCP or SDK layer.

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
