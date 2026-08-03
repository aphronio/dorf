# Dorf

Dorf is a local-first control plane for durable AI Workers in isolated Rooms.

The project is licensed under the
[Apache License 2.0](https://github.com/aphronio/dorf/blob/main/LICENSE). The runtime and Python SDK remain
experimental and do not yet carry a third-party compatibility promise. See
[Internal Worker, Room, and Job Runtime](https://github.com/aphronio/dorf/blob/main/docs/project/runtime.md) for the current responsibility
boundary.

```text
Worker ──current Room──> Room
Job ──Assignment──> Worker
```

A Worker is a durable identity with a harness-native general conversation. Its current Room is the
isolated execution body that holds its workspace and native history. A Job exists only with complete
goal version 1 and has a separate native conversation connected to one Worker by an explicit
Assignment. Worker and Job inputs have
independent durable FIFOs and may execute concurrently.

The portable lifecycle lives in `dorf.runtime`; the concrete in-process facade lives in
`dorf.sdk`; built-in Codex and Incus implementations live under `dorf.adapters`;
coding-to-PR policy lives in `dorf.workflows`. Runtime code does not import coding, GitHub,
Incus, or Codex policy.

The CLI's Worker and Job resource commands use the facade that embedded clients can call directly:

```python
from dorf import Dorf

with Dorf.open() as dorf:
    inspection = dorf.inspect_worker("researcher")
    receipt = dorf.message_worker(
        "researcher",
        "Profile the API first",
        action_id="caller-stable-action-id",
    )

with Dorf.open() as dorf:
    outcome = dorf.wait_for_worker_message(
        "researcher",
        message_id=receipt.message.id,
        timeout=30,
    )
```

Remote Rooms remain an Environment-adapter concern. Dorf itself does not need to become a
network service merely because a future adapter controls cloud VMs.

## Current focus

The only current requirements driver is coding-to-PR with Codex in local Incus VMs. Repo-owned
commands prepare and verify the workspace, and GitHub PRs provide review and acceptance. Other
workflows, harnesses, environment providers, Worker pools, scheduling, and cross-Worker reassignment
are not current capabilities.

The shared [Provider Gateway](https://github.com/aphronio/dorf/blob/main/docs/project/provider-gateway.md) provides a directly importable local facade,
one pinned persistent broker, named ChatGPT-subscription and OpenAI-Platform connections, typed
status/remediation, and revocable consumer-specific routes. Credential-free Codex Rooms use that
validated D035 route through the private Incus bridge. Later providers become supported only after
their real authentication and wire paths are exercised.

```python
from dorf.provider_gateway import ProviderGateway

with ProviderGateway.open() as gateway:
    connections = gateway.list_connections()
    status = gateway.connection_status("personal-chatgpt")
    route = gateway.create_route("personal-chatgpt", consumer="my-client")
```

The resource CLI reaches the same authority:

```bash
uv run dorf provider connect chatgpt --subscription --name personal-chatgpt
OPENAI_API_KEY=... uv run dorf provider connect openai --api-key --name work-openai
uv run dorf provider list
uv run dorf provider status personal-chatgpt
uv run dorf provider disconnect personal-chatgpt
```

The ChatGPT command prints a short-lived device URL and code for headless login. The OpenAI command
reads its key from `OPENAI_API_KEY` or a hidden prompt; it never accepts the key as an option value.
Stored upstream credentials, broker paths, and route keys are absent from list/status output.
Connecting also selects that named connection as the global default for new Rooms without copying
its credential into Dorf configuration.
`dorf doctor` inspects bounded gateway and Incus health without installing or starting the
broker.

One coding task composes:

```text
one dedicated coder-JOB Worker
+ one goal-backed Job and Assignment
+ one independent clone at /workspace/jobs/JOB
+ one branch and PR proposal
```

Human-requested revision continues the same Worker, Room, Job, Assignment, clone, branch, and PR.
Successful turns, checks, PR creation, and Worker completion claims leave the Job open. Merge,
explicit rejection, and abandonment end the coding Job; dedicated coding Workers are then ended and
their Rooms destroyed. Caller-managed Workers return idle after their Job ends.

## Runtime commands

CLI help is the syntax source of truth:

```bash
uv run dorf --help
uv run dorf worker --help
uv run dorf job --help
uv run dorf provider --help
```

Create an independent Worker and initial Room without creating a Job or Job documents:

```bash
uv run dorf worker spawn researcher
```

Direct Worker conversation and human Room entry:

```bash
uv run dorf worker message researcher "What can you help with?"
uv run dorf worker wait researcher
uv run dorf worker inspect researcher
uv run dorf worker attach researcher
```

`worker attach` resolves the Worker's current Room and opens an interactive shell at `/workspace`.
Exit the shell with `Ctrl-D` or `exit` to leave. Active human presence is transactional runtime state;
ending the shell clears it. There is no separate detach command or resident tmux identity. A second
concurrent attachment is rejected, and raw `incus exec` remains available for break-glass access.

Recover replaceable controllers and the exact current Room through the same Worker identity:

```bash
uv run dorf worker recover researcher
```

Recovery preserves a usable or stopped Room, its workspace, and its harness-native history.
Unsettled Worker and Job inputs are reconciled against native turn history before dispatch resumes;
they are never blindly resent. If the recorded Incus VM is absent, Dorf records the exact Room as
lost and leaves the Worker offline without creating a replacement Room, Assignment generation,
coding clone, or native thread. Durable identity and queued input remain inspectable, but continuing
requires a fresh Worker and Job.

The first direct message lazily creates the Worker-general native conversation and pins its model
and reasoning defaults. Later messages may override either per turn. An unavailable Worker still
accepts durable input; delivery remains pending until recovery.

Create a Job only when its exact goal can be pinned and assigned to an existing ready Worker:

```bash
uv run dorf job assign checkout-perf \
  --to researcher \
  --goal "Make checkout instant and leave evidence"
```

Job conversation and read-only lenses:

```bash
uv run dorf job message checkout-perf "Profile the API first"
uv run dorf job wait checkout-perf
uv run dorf job inspect checkout-perf
uv run dorf job inspect checkout-perf --timeline
uv run dorf job inspect checkout-perf --evidence
uv run dorf job artifact list checkout-perf
uv run dorf job artifact export checkout-perf ARTIFACT_REF --to ./exports
uv run dorf job end checkout-perf
uv run dorf worker end researcher
```

Normal Job ending requires settled input and runs one stable cooperative cleanup turn before removing
the Assignment workspace and returning a caller-managed Worker to idle. `--interrupt` explicitly
cancels unsettled work and bypasses that cooperative turn. Worker ending requires no open Job,
destroys the exact current Room, and retains an ended identity. Cleanup failure remains visible and
retryable through the same command. If a Room is proven absent, `job end --interrupt` acknowledges
that loss without attempting native or Room-local cleanup; the Roomless Worker can then be ended.

`wait` pins the latest admitted input at invocation, or an exact `--message ID`. It never sends a
status prompt or starts delivery. Pulse, timeline, and evidence inspection remain usable when the
Room is unavailable and do not ingest reports or launch collectors.

Job-native turns receive exact Job, Assignment, workspace, context, and report-outbox identity.
Worker-general turns receive none of those values or reporting guidance. Workers may publish bounded
milestone, assumption, completion, and artifact claims through `dorf-report`; an
Assignment-fenced collector validates them into immutable Job timeline and evidence documents.
Worker claims remain claims, while runtime and workflow observations are facts.

The artifact manifest uses stable Job-scoped references and never exposes the retained blob's host
path. Export copies the digest-verified original bytes into an existing caller-selected directory
under the recorded safe filename, including binary files and files larger than the model-readable
64 KiB limit. An existing destination is refused unless `--overwrite` is explicit, and the final
filename appears only after the complete size and digest have been verified.

The removed top-level `spawn`, `assign`, `send`, `inspect`, and `wait` commands are not aliases. Use
the typed `worker` and `job` groups.

## Coding workflow

From a clean configured repository:

```bash
uv run dorf start "Implement the task" \
  --provider-connection personal-chatgpt
uv run dorf start --issue 139 \
  --provider-connection personal-chatgpt
# after a reported clone/setup failure only:
uv run dorf start "Implement the task" --resume JOB \
  --provider-connection personal-chatgpt
```

`start` creates deterministic Worker `coder-JOB` with explicit `coding-workflow` provenance and
`dedicated` lifecycle policy, creates the exact goal-backed Job/Assignment, and clones the repository
independently into `/workspace/jobs/JOB`. It does not use Git worktrees or treat `/workspace` as the
Job root.

Higher-level workflow commands target the coding Job name:

```bash
uv run dorf status JOB
uv run dorf implementation-status JOB --wait
uv run dorf check JOB
uv run dorf smoke JOB
uv run dorf review JOB
uv run dorf verify JOB
uv run dorf publish JOB
uv run dorf followup JOB
uv run dorf runs JOB
uv run dorf show-run JOB RUN_ID
uv run dorf complete JOB   # record merged or rejected PR truth
uv run dorf discard JOB    # record explicit abandonment
```

`dorf afk ISSUE --provider-connection NAME` and `dorf afk-resume JOB` are workflow
orchestration over the same Worker and Job resources, not a second runtime model.

Repositories configure deterministic commands, review commands, Incus settings, selected host
environment bindings, and conversation defaults in `.dorf.toml`. Workflow command output is
stored as a workflow fact and attached to the Job evidence plane. Git and GitHub remain authoritative
for branches, commits, and PR state; Codex remains authoritative for native transcript history.

Local operational state defaults to:

```text
~/.local/share/dorf/state.sqlite3
~/.local/share/dorf/jobs/
~/.local/share/dorf/runs/
~/.local/share/dorf/locks/
```

The cutover intentionally did not migrate the superseded experimental Session schema.

## Project docs

- [North Star](https://github.com/aphronio/dorf/blob/main/docs/project/north-star.md)
- [Showcase Ideals](https://github.com/aphronio/dorf/blob/main/docs/project/showcase-ideals.md)
- [Principles](https://github.com/aphronio/dorf/blob/main/docs/project/principles.md)
- [Decision Log](https://github.com/aphronio/dorf/blob/main/docs/project/decisions.md)
- [Internal Worker, Room, and Job Runtime](https://github.com/aphronio/dorf/blob/main/docs/project/runtime.md)
- [Core Setup and Summon DX](https://github.com/aphronio/dorf/blob/main/docs/implementation/core-setup.md)
- [Setup Support](https://github.com/aphronio/dorf/blob/main/docs/support.md)
- [Incus Image](https://github.com/aphronio/dorf/blob/main/docs/implementation/incus-image.md)
- [Release process](https://github.com/aphronio/dorf/blob/main/docs/releasing.md)

Historical material under `docs/research/` is non-normative.
