# Agent guide

Use this runbook when a human delegates Dorf installation or operation to an agent. First determine
whether this machine is the deployment host or a remote CLI client; those roles have different
authority. [Getting started](getting-started.md) owns procedures and
[Support and diagnostics](support.md) owns platform limits and fault attribution.

## Role boundary

The **deployment-host agent** may install Dorf, run setup, manage Profiles and optional integrations,
operate diagnostics, and coordinate the operator-owned API, worker, storage, provider, Harness, and
ingress boundaries. It must pause for every password, secret, browser authorization, paid service,
or consequential infrastructure choice.

The **remote-client agent** installs and uses only the Dorf CLI. It may connect, check its own
authentication, admit a direct Job, inspect that Job, and request its cleanup. It must not run
`dorf setup`, `dorf serve`, `dorf worker`, database or migration commands, `dorf provider`,
`dorf profile`, or GitHub integration setup. It must not use SSH or local-only commands to work
around a missing remote capability. The deployment host owns all of those operations.

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
prepare the approved Sandbox and Harness path; do not substitute direct Docker, Incus, PostgreSQL,
systemd, E2B, Cloudflare, or Gateway mutations for its recovery. Run the documented Profile,
Provider Gateway, and `dorf doctor` checks with the exact selected names.

Optional integrations remain host concerns. Pause while the human completes browser authorization,
and return a short-lived code only to its waiting command. Let runtime composition supply integration
authority instead of putting credentials or integration settings in a Job request.

Follow the host-service boundary and current lifecycle limits in the remote-client section of
Getting started. Do not turn a repository dogfood helper into an operator interface or infer
readiness from a terminal merely remaining open.

## Remote-client protocol

Receive the exact HTTPS Deployment origin and Enrollment through the human-approved private handoff,
then follow the remote-client procedure in Getting started. One successful `dorf connect` saves one
Deployment; there are no contexts to choose or switch. Prove the binding with `dorf auth status`.

Put the complete goal in a file and use `dorf run`. Admission retry identity is automatic; do not
invent or ask the human to manage a key in the ordinary flow. Observe only with `dorf job inspect`
and request cleanup only with `dorf job cleanup`. An open Job may be idle after a successful Turn;
do not create a replacement merely because it has no active execution. Remote Job listing, watch,
Messages, explicit Job retry, file retrieval, Evidence, and workflow admission are not delivered
yet.

## Safety and handback

Never expose Enrollment codes, Client credentials or configuration, environment dumps, Gateway
state, provider credentials, or Harness transcripts. Never delete provider resources or PostgreSQL
rows directly. Stop and report the exact failed check when repair needs authority the human has not
granted.

Report only the installed version, the role operated, readiness actually observed, Job identity and
current state, caller-requested output, and cleanup state. A deployment-host handback may also name
the selected Profile and AI connection. Do not claim installation, work, authentication, or cleanup
succeeded merely because a command was started; use the factual CLI result for that role.
