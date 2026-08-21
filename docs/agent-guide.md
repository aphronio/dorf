# Agent guide

Use this runbook when a human delegates Dorf installation or operation to an agent. The supported
deployment and command details remain authoritative in [Getting started](getting-started.md), while
[Support and diagnostics](support.md) owns platform limits and fault attribution.

## Suggested handoff

```text
Install and operate Dorf on this machine by following docs/agent-guide.md. Coordinate the supported
dorf CLI instead of configuring its database, containers, Sandboxes, Gateway, or tunnels by hand.
Pause for every password, secret, browser authorization, or consequential provider choice. Never
read, print, or copy credentials into chat. Finish by reporting only observed readiness, Job,
Artifact or Proposal, attention, and cleanup facts.
```

## Installation protocol

1. Confirm that this is a supported x86_64 Linux host and read the two documents linked above. Do
   not infer requirements from `docs/research/` or `docs/history/`.
2. Use an immutable verified release binary. In a contributor checkout, use the repository-owned
   build commands in [CONTRIBUTING.md](../CONTRIBUTING.md) only when the human explicitly wants a
   source build. Run `dorf version` before setup.
3. Run `dorf setup`. Let its interactive flow select local Incus, cloud E2B, both, or neither. Do not
   add `--yes` until the human has approved the exact displayed host or Cloudflare changes.
4. Pause and let the human perform these boundaries personally:

   - enter a sudo password;
   - complete ChatGPT or Cloudflare browser authorization;
   - enter an OpenAI or E2B key through the masked prompt or an explicitly supplied protected file;
   - choose infrastructure, a DNS hostname, paid services, or broader network access.

5. If setup requests a new login or stops after a recoverable failure, preserve its state and rerun
   the same `dorf setup` command. Do not replace its recovery with direct Docker, Incus, PostgreSQL,
   systemd, E2B, or Cloudflare mutations.
6. Prove the resulting named profile and model route with the exact names selected during setup:

   ```bash
   dorf profile list
   dorf provider status --profile PROFILE --ai-connection AI_CONNECTION --json
   dorf doctor --profile PROFILE
   ```

   A repository Job needs the additional GitHub authority checks documented in Getting started.

## Operating protocol

- Put complete goals, briefs, and follow-up Messages in files. Use a stable caller key or request ID
  so a lost response can be retried without creating competing work.
- Use the documented `dorf admit` command for coding or
  `dorf workflow run codebase-investigation` for an investigation. Do not reproduce workflow policy
  with ad-hoc Sandbox commands.
- Keep one `dorf worker` running in a persistent terminal. Watching is optional; use
  `dorf inspect --follow JOB_ID` when the human wants a live view.
- Send later input with `dorf message --job JOB_ID --id REQUEST_ID --input-file FILE`. Use
  `--intent steer` only when the human explicitly wants to redirect active work.
- When inspection reports attention, repair the reported cause and use `dorf retry JOB_ID`. Retry
  delegates eligibility to the same durable task; it is not a request to create a replacement Job.
- Discover deliverables with `dorf artifact list JOB_ID` and retrieve exact bytes with
  `dorf artifact get ARTIFACT_ID`. Never treat agent prose alone as verification.
- Request `dorf cleanup JOB_ID` only when the workflow or human has decided that its resources may
  be released. Outcome and cleanup are separate facts.

## Safety and handback

Never expose credentials, environment dumps, Gateway state, or Harness transcripts. Never delete
provider resources or PostgreSQL rows directly, and never substitute a disposable Quick Tunnel for
the stable Gateway contract. Stop and report the exact failed check when a repair needs authority
the human has not granted.

At handback, report:

- installed Dorf version and selected Sandbox profile and AI connection;
- readiness checks that are actually `ready`;
- Job ID and current work or attention, if a Job was admitted;
- retained Artifact ID or Proposal URL, when present; and
- cleanup state.

Do not say installation, work, or cleanup succeeded merely because a command was started. Use the
facts returned by `dorf provider status`, `dorf doctor`, and `dorf inspect`.
