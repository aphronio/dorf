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
attention, caller-requested output, and cleanup facts.
```

## Installation protocol

1. Confirm that this is a supported x86_64 Linux host and read the two documents linked above. Do
   not infer requirements from `docs/research/` or `docs/history/`.
2. Use the installer from an immutable release, or the manual attested-release fallback documented
   in Getting started. In a contributor checkout, use the repository-owned build commands in
   [CONTRIBUTING.md](../CONTRIBUTING.md) only when the human explicitly wants a source build. Follow
   any printed `PATH` handoff and run `dorf version` before setup. The installer must not run setup.
   Use `dorf update` for later upgrades through the same verified installer path.
3. Run `dorf setup`. Let its interactive flow select local Incus, cloud E2B, both, or neither. Do not
   add `--yes` until the human has approved the exact displayed host or Cloudflare changes.
4. Pause and let the human perform these boundaries personally:

   - enter a sudo password;
   - complete ChatGPT, Cloudflare, or optional-integration browser authorization;
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

   When the requested operation needs an optional external integration, use that integration's
   setup command after the shared foundation is ready. Pause while the human approves any browser
   form, and return any short-lived redirected URL or code only to the waiting command. Never copy it
   into chat. Let the runtime operation prove its own exact authority; do not put optional
   integration settings in Job requests because the selected profile's runtime composition supplies
   them.

## Operating protocol

- Start with `dorf help` and the relevant subcommand help. Treat those commands and their current
  output as the CLI authority; do not copy workflow-specific recipes into this guide or reproduce
  workflow policy with ad-hoc Sandbox commands.
- Put complete goals, briefs, and follow-up Messages in files. Use a stable caller key or request ID
  so a lost response can be retried without creating competing work.
- Keep one Dorf worker running in a persistent terminal. Watching is optional; inspect the Job when
  the human wants a live view or when a command reports attention.
- An open Job may have no current operation while retaining its execution owner for later input. Do
  not retry or create a replacement Job merely because it is idle.
- Repair the reported cause before retrying attention. Retry delegates eligibility to the same
  durable task; it does not create replacement work.
- Read any caller-required Sandbox files before requesting cleanup. Never treat agent prose alone as
  verification. Cleanup is explicit and separate from workflow outcome.

## Safety and handback

Never expose credentials, environment dumps, Gateway state, or Harness transcripts. Never delete
provider resources or PostgreSQL rows directly, and never substitute a disposable Quick Tunnel for
the stable Gateway contract. Stop and report the exact failed check when a repair needs authority
the human has not granted.

At handback, report:

- installed Dorf version and selected Sandbox profile and AI connection;
- readiness checks that are actually `ready`;
- Job ID and current work or attention, if a Job was admitted;
- any output the human explicitly asked the agent to retrieve; and
- cleanup state.

Do not say installation, work, or cleanup succeeded merely because a command was started. Use the
facts returned by `dorf provider status`, `dorf doctor`, and `dorf inspect`.
