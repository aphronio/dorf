# Delegate Dorf operation to an agent

Use this guide when a human delegates Dorf installation or operation to an agent. This guide owns
the agent-specific authority, human pauses, safety rules, and handback. It does not repeat operator
commands.

[Getting started](getting-started.md) owns the procedures. [Support and diagnostics](support.md)
owns supported platforms, readiness, and fault attribution. Follow those authorities exactly. Do
not infer a procedure from `docs/research/` or `docs/history/`.

## Choose one operating role

| Role | Granted authority | Boundary |
| --- | --- | --- |
| Deployment-host agent | Install Dorf, run setup, use documented Compose, Job, and workflow operations, manage Profiles and optional integrations, and run diagnostics. | Pause for each secret, browser authorization, paid service, administrator helper, DNS replacement, or consequential infrastructure choice. |
| Remote-client agent | Install and use the Dorf CLI, connect one Client, and use documented Job and workflow operations. | Do not run deployment-host commands, access PostgreSQL, operate the host Compose project, or use SSH or local commands to bypass a missing remote capability. |

Do not combine the roles unless the human explicitly grants both. The deployment host retains its
own credentials and infrastructure authority when a remote-client agent operates Jobs.

## Open the procedure for the task

| Task | Procedure |
| --- | --- |
| Install Dorf or initialize and operate a deployment host | [Initialize a deployment host](getting-started.md#1-install-the-application-initialize-a-deployment-host) |
| Prepare a separate Incus workstation | [Prepare a remote Incus workstation](getting-started.md#prepare-a-remote-incus-workstation) |
| Configure the optional GitHub integration | [Set up the GitHub integration](getting-started.md#2-set-up-the-optional-github-integration) |
| Connect or use a remote CLI Client | [Connect a remote CLI Client](getting-started.md#3-connect-one-remote-cli-client) |
| Run a direct Job on the deployment host | [Run a direct Job](getting-started.md#4-run-a-direct-job-on-the-deployment-host) |
| Run a coding Job on the deployment host | [Run a coding Job](getting-started.md#5-run-a-coding-job-on-the-deployment-host) |
| Diagnose installation, readiness, authentication, Job, or cleanup failure | [Support and diagnostics](support.md) |
| Call the service directly from code | [Remote Control API](control-api.md) and its deployment-published OpenAPI document |

Use the exact procedure for the selected role. If the machine, operating system, network, provider,
or requested operation falls outside the documented path, stop and report the unsupported boundary.
Do not improvise a broader topology or authority grant.

## Protect human authority and credentials

Never read, print, copy, or return an Enrollment code, Client credential or configuration, provider
credential, Gateway state, environment dump, Harness transcript, or unrequested Job output. Let the
human enter a secret at the CLI prompt. You may consume a protected file or standard input only when
the human explicitly supplies it for that purpose.

Pause before any browser authorization, paid service, administrator helper, DNS replacement, or
other consequential infrastructure choice. Show the human the exact requested action and resume
only after approval. An available command or credential does not grant broader authority.

On a deployment host, do not edit the generated `.env`, invent another lifecycle wrapper, start
separate foreground Dorf processes, or mutate database rows and provider resources directly. Setup
does not grant permission to install host prerequisites. If setup offers an administrator helper,
let the human inspect and authorize that exact helper before it runs.

On a remote client, use the documented CLI or the published HTTPS contract. Do not recover a missing
operation through SSH, local-only commands, direct database access, or deployment-host credentials.

## Verify and hand back

Use factual command results and external observations. A started command, open terminal, process,
or completed agent Turn does not prove installation, work, authentication, or cleanup succeeded.
Follow the owning procedure through its terminal check.

Return only the information the human needs:

- The installed Dorf version and the role you operated.
- Readiness that you observed directly.
- The requested Job identity, current state, and requested result.
- The selected Profile or AI connection only for a deployment-host handback.
- Cleanup state and any unresolved failure or missing authority.

Redact caller input before reporting diagnostics. Do not include credentials, complete inspection
or watch output, unrelated Message output, or private Harness history.
