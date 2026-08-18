# Dorf

**Dorf is the open-source control plane for running agent harnesses on infrastructure you control.**

Your agents. Your infrastructure. One API.

Dorf's direction is to carry a supported agent setup into compatible isolated infrastructure
without rebuilding it in a new agent framework. Dorf keeps custody of the whole Job, including
recovery, external effects, evidence, the workflow-defined outcome, and cleanup.

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
| Choice of agent | Codex or Pi |
| Choice of Sandbox | Local Incus; E2B proof profile |
| Many ways to start Jobs | CLI |

## Build

Dorf's default supported deployment runs on x86_64 Linux with local Incus. An admitted E2B proof
profile requires an exact template and a deployment-owned HTTPS Provider Gateway route. Disposable
tunnels are not a supported deployment. macOS is not supported as a host.

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf version
```

Releases contain the same x86_64 Linux binary. See
[Getting started](docs/getting-started.md) for PostgreSQL, Incus, the credential-free Sandbox image,
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

The common Sandbox seam has been proved with Incus and E2B. Broader deployment profiles and general
workflow authoring remain use-case-driven work.

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
scripts/dev/setup.sh
.dorf/bin/mise run check
.dorf/bin/mise exec -- scripts/build-release.sh dist/release
```

Architecture and authority details are indexed in [docs/README.md](docs/README.md).
