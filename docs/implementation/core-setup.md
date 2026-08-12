# Go application setup

The supported setup path is the x86_64 Linux Go application described in
[Getting started](../getting-started.md). It converges native PostgreSQL and Absurd, a local Incus
daemon and credential-free Codex image, the pinned Provider Gateway, GitHub App authority, and a
repository contract. It does not create setup state, run a workflow engine, contact a cloud
durability service, use the host Docker socket, or invoke Python.

## Boundaries

- `dorf setup --provider NAME` initializes the pinned Absurd schema when necessary, applies Dorf's
  PostgreSQL schema, and runs direct readiness checks.
- `dorf provider connect chatgpt --name NAME` installs the verified CLIProxyAPI 7.2.104 x86_64
  binary under the Provider Gateway state directory, binds it to the exact private Incus bridge,
  completes device authentication, and enables the Responses WebSocket capability used by Codex.
- `dorf image install --manifest FILE --archive FILE` verifies and imports the release's
  credential-free x86_64 Incus VM.
- `dorf doctor` reports bounded facts and remediation for PostgreSQL, Absurd, Incus, the image,
  Provider Gateway, optional repository contract, and optional GitHub repository authority.

Host package installation remains an explicit administrator operation. Ubuntu 24.04 is the proven
recipe; operators with an already usable local Incus daemon need no distribution mutation. Setup
does not commandeer partially configured Incus storage or networking.

## Exact support posture

The official artifact and Incus image are x86_64 Linux only. macOS cannot host the local Incus VM
daemon and is unsupported; Dorf does not invent a remote-Sandbox mode to claim parity. Another
Linux distribution is supported only at the capability boundary after its operator installs a
working local Incus daemon.

## Diagnosis

Readiness checks are observations, not completion flags. Rerunning setup rechecks current state and
performs only idempotent PostgreSQL/Absurd initialization. Failures retain separate ownership and
one concrete remediation. No raw provider credential, scoped route key, GitHub private key, Codex
control capability, transcript, or arbitrary environment dump is rendered.

## Clean-machine proof limit

The historical Ubuntu 24.04 Incus/image/provider terminals remain evidence for the native
dependencies. Issue #38's repository proof can establish the static Go artifact, schema bootstrap,
unit behavior, and bounded diagnostics in this VM. A fresh nested Incus host, live ChatGPT device
authorization, and GitHub App authority are infrastructure/external boundaries and must be recorded
by the final orchestrator rather than inferred from tests.
