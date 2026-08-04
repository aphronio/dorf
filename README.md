# Dorf

**A control plane for durable AI workers on infrastructure you control.**

Dorf gives an AI worker a durable identity, an isolated Room, and an explicit Job. Start work,
leave, reconnect, inspect what changed, steer it, or clean it up without treating one terminal or
client process as the source of truth.

Workers do Jobs in Rooms. A **Worker** is the durable harness identity, a **Room** is its isolated
execution boundary, a **Job** is a pinned goal with its own conversation and evidence, and an
**Assignment** records which Worker and Room own that Job.

> [!IMPORTANT]
> Dorf is alpha software. The current verified path is Codex in local Incus VMs on x86_64 Linux,
> with automatic host convergence reviewed on Arch Linux and Ubuntu 24.04 LTS. No other harness or
> Room backend is supported yet.

## Why Dorf

- **Durable work, replaceable processes.** Worker and Job identity outlive a CLI invocation,
  controller restart, or disconnected client.
- **Owned isolation.** Work runs in a private VM Room instead of sharing the host Docker socket or
  an unbounded host shell.
- **Detached by default.** Admit a message once, leave, and later inspect the same Worker or Job
  rather than babysitting a token stream.
- **Explicit lifecycle.** Recovery and cleanup operate on recorded Worker, Room, Job, and
  Assignment identity; failures remain visible and retryable.
- **A composable runtime.** The control plane owns mechanisms. Applications own workflow policy,
  acceptance, and presentation.

The product direction is broader than the first adapter pair, but the support claim is deliberately
narrow. Dorf adds another harness or Room backend only after a real implementation validates the
seam. See the
[North Star](https://github.com/aphronio/dorf/blob/main/docs/project/north-star.md) for the
destination and
[Runtime Surface](https://github.com/aphronio/dorf/blob/main/docs/project/runtime.md) for what
exists now.

## Available today

| Layer | Current support |
| --- | --- |
| Runtime | Durable Workers, Rooms, Jobs, Assignments, message admission, inspection, recovery, evidence, and cleanup |
| Agent harness | Codex app-server |
| Room backend | Local Incus virtual machines |
| Model access | Named ChatGPT-subscription and OpenAI API-key connections through the local Provider Gateway |
| Host setup | x86_64 Arch Linux and Ubuntu 24.04 LTS convergence; other x86_64 Linux hosts may work with an already usable Incus installation |
| Dogfood application | Coding-to-PR, including isolated clones, repo-owned checks, review, follow-up, and PR proposal |

Multiple harnesses, alternative sandbox providers, remote Room backends, Worker pools, scheduling,
and cross-Worker reassignment are direction—not current capabilities.

## Install the alpha CLI

Install from PyPI with:

```bash
uv tool install dorf
dorf --version
dorf --help
dorf setup
```

`dorf setup` inspects the host, guides supported Incus installation, downloads the immutable
Codex-harness VM image attached to the Dorf release, connects an AI model provider, and proves one
real disposable Worker turn before reporting ready.

To work from source:

```bash
git clone https://github.com/aphronio/dorf.git
cd dorf
uv sync --all-groups
uv run pytest
uv run ruff check .
uv run dorf --help
```

## Core loop

On a configured host, create a Worker and assign a Job with a complete goal:

```bash
dorf worker spawn my-worker
dorf job assign checkout-perf \
  --to my-worker \
  --goal "Make checkout feel instant and leave evidence"
```

Message and inspect the Worker or its Job independently:

```bash
dorf worker message my-worker "What can you help with?"
dorf worker wait my-worker
dorf worker inspect my-worker

dorf job message checkout-perf "Profile the API first"
dorf job wait checkout-perf
dorf job inspect checkout-perf
dorf job inspect checkout-perf --timeline
dorf job inspect checkout-perf --evidence
```

Enter the current Room when direct takeover is useful:

```bash
dorf worker attach my-worker
```

End the Job before ending its Worker. Cleanup is bound to the exact recorded resources:

```bash
dorf job end checkout-perf
dorf worker end my-worker
```

The same authority is available in process through the typed Python facade:

```python
from dorf import Dorf

with Dorf.open() as dorf:
    inspection = dorf.inspect_worker("my-worker")
    receipt = dorf.message_worker(
        "my-worker",
        "Profile the API first",
        action_id="caller-stable-action-id",
    )
```

Run `dorf worker --help` and `dorf job --help` for the complete current command surface.

## Coding-to-PR showcase

Coding-to-PR is Dorf's current dogfood application, not the runtime's identity. One coding task
composes one goal-backed Job, Assignment, isolated clone, branch, and PR proposal:

```bash
dorf start "Implement the task" --provider-connection personal-chatgpt
dorf status JOB
dorf verify JOB
dorf publish JOB
dorf complete JOB
```

Repositories declare deterministic setup, check, smoke, and review commands in `.dorf.toml`. Git
and GitHub remain authoritative for branches, commits, and PR state; the harness remains
authoritative for its native conversation history.

## Architecture boundary

```text
dorf.runtime     portable Worker, Room, Job, and Assignment mechanisms
dorf.sdk         in-process facade used by the CLI and host applications
dorf.adapters    current Codex and Incus integrations
dorf.workflows   coding-to-PR policy composed over the runtime
```

Runtime code does not import coding, GitHub, Incus, or Codex policy. Remote Rooms remain an adapter
concern; Dorf does not need to become a hosted service merely because an adapter controls remote
infrastructure.

## Documentation

- [North Star](https://github.com/aphronio/dorf/blob/main/docs/project/north-star.md) — approved product direction
- [Principles](https://github.com/aphronio/dorf/blob/main/docs/project/principles.md) — product and engineering judgment
- [Runtime Surface](https://github.com/aphronio/dorf/blob/main/docs/project/runtime.md) — current portable boundary
- [Decision Log](https://github.com/aphronio/dorf/blob/main/docs/project/decisions.md) — accepted choices and reconsideration triggers
- [Provider Gateway](https://github.com/aphronio/dorf/blob/main/docs/project/provider-gateway.md) — AI model provider and scoped route boundary
- [Setup support](https://github.com/aphronio/dorf/blob/main/docs/support.md) — current host support and diagnostics
- [Contributing](https://github.com/aphronio/dorf/blob/main/CONTRIBUTING.md) — development workflow and DCO sign-off
- [Security policy](https://github.com/aphronio/dorf/blob/main/SECURITY.md) — private vulnerability reporting

Dorf is licensed under the
[Apache License 2.0](https://github.com/aphronio/dorf/blob/main/LICENSE). The runtime and Python SDK
remain experimental and do not yet carry a third-party compatibility promise. Material under
`docs/research/` is archival and non-normative.
