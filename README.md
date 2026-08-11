# Dorf

**Dorf is the open-source control plane for durable agent Jobs on infrastructure you control.**

Dorf uses code for predictable work and isolated agents for judgment. Jobs can run in the
background, recover from failure, and show evidence of what happened.

Today, Dorf supports one coding workflow: a Job starts with a goal and ends with a verified pull
request.

```text
Coding Job
    |
    v
+------------------------------------------------------------+
|                    Dorf coding workflow                    |
|                                                            |
|  Isolate -> Implement -> Verify -> Review -> Pull request  |
|                                                            |
|       Durable recovery and evidence across every step      |
+------------------------------------------------------------+
```

| Direction | Works today |
| --- | --- |
| Many kinds of Jobs | Coding Job to PR |
| Choice of agent | Codex |
| Choice of Sandbox | Local Incus on x86_64 Linux |
| Many ways to start Jobs | CLI |

## Build

Dorf currently runs on x86_64 Linux with local Incus. macOS is not supported as a host.

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf version
```

Releases contain the same x86_64 Linux binary. See
[Getting started](docs/getting-started.md) for PostgreSQL, Incus, the credential-free Codex image,
Provider Gateway, GitHub App, and repository preparation.

## Product model

```text
A client starts a Job with a clear goal.
Code handles predictable steps.
Agents handle judgment inside an isolated Sandbox.
Dorf records evidence and cleans up resources.
```

Each coding Job gets its own Sandbox, clone, branch, checks, review, and pull request. Dorf can resume
the Job without repeating completed work.

Coding is the first workflow, not the final limit. A second real workflow, such as research, will
show which parts should become a shared API.

The main commands are:

```bash
dorf setup --provider personal-chatgpt
dorf doctor --provider personal-chatgpt
dorf admit ...
dorf worker
dorf message --job JOB --id STABLE_ID --input-file message.txt --intent steer
dorf inspect JOB
dorf abandon JOB
dorf cleanup JOB
```

`inspect` shows what happened, what is running, what needs attention, and what still needs cleanup.
Use `absurdctl list-tasks` and `absurdctl dump-task` for scheduler details.

## Development

The project is one Go application with generated PostgreSQL query code:

```bash
scripts/dev/prepare.sh
scripts/dev/check.sh
scripts/build-release.sh dist/release
```

Architecture and authority details are indexed in [docs/README.md](docs/README.md).
