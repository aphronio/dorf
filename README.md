<picture>
  <source media="(prefers-reduced-motion: reduce)" srcset="assets/cover.png">
  <img alt="Workers continuing their jobs inside isolated Rooms in the Dorf village" src="assets/cover.gif">
</picture>

# Dorf

**Dorf is the open-source control plane for running agent harnesses on infrastructure you control.**

Your agents. Your infrastructure. One API.

Dorf's direction is to carry a supported agent setup into compatible isolated infrastructure
without rebuilding it in a new agent framework. Dorf keeps custody of controlled execution,
including recovery, external effects, retained results, and requested cleanup.

```text
External clients                  Native workflows
        |                                |
        +-------------+------------------+
                      v
                 Dorf Core
        durable execution and recovery
                      |
                      v
       Sandbox providers x agent Harnesses
```

Dorf is a stateful, self-hosted control plane, not an agent framework or an embeddable runtime SDK.
Native workflows consume Core in-process. External clients use the CLI today; a public network API
and thin client SDKs are direction, not current support claims. Clients may eventually drive Core
directly or delegate policy to a predefined workflow.

The built-in workflows currently cover coding to a verified pull-request Proposal and
repository-grounded codebase investigation. See [Getting started](docs/getting-started.md) for the
supported deployment, profiles, commands, and workflow inputs. To hand installation or operation to
an agent, point it at the concise [Agent guide](docs/agent-guide.md).

## Build

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf version
```

## Development

The project is one Go application with generated PostgreSQL query code:

```bash
scripts/dev/setup.sh
.dorf/bin/mise run check
.dorf/bin/mise exec -- scripts/build-release.sh dist/release
```

Architecture and authority details are indexed in [docs/README.md](docs/README.md).
