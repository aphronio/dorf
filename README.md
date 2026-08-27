<picture>
  <source media="(prefers-reduced-motion: reduce)" srcset="assets/cover.png">
  <img alt="Workers continuing their jobs inside isolated Rooms in the Dorf village" src="assets/cover.gif">
</picture>

<p align="center"><strong>Your agents. Your infrastructure. One API.</strong></p>

# Dorf

**Dorf is the open-source control plane for running agent harnesses on infrastructure you control.**

Dorf's direction is to carry a supported agent setup into compatible isolated infrastructure
without rebuilding it in a new agent framework. Dorf keeps custody of controlled execution,
including recovery, external effects, retained results, and requested cleanup.

```text
Deployment-host CLI       Remote clients       Native workflows
        |                       |                      |
 loopback HTTP        operator HTTPS ingress   in-process composition
        |                       |                      |
        +-----------------------+-----------+----------+
                                            v
                                    Dorf deployment
        durable execution and recovery
                      |
                      v
       Sandbox providers x agent Harnesses
```

Dorf is a stateful, self-hosted control plane, not an agent framework or an embeddable runtime SDK.
Native workflows compose Core in-process. The client boundary is intentionally narrow: an enrolled
CLI can admit a direct Job or either fixed built-in workflow and operate their common interaction
loop—Messages, observation, eligible recovery, exact Sandbox files, verified Evidence metadata,
cleanup, and bounded Job listing—through one configured Dorf deployment. The deployment-host CLI
uses fixed authenticated loopback HTTP; remote clients use operator-owned HTTPS ingress. Each
deployment publishes its OpenAPI and typed Problem catalog. Static release manifests define
separately supervised API and worker services; one resumable `dorf setup` flow prepares their
protected configuration and applies that exact Compose project. Operators use Compose directly only
for advanced lifecycle operations. Generic workflow registration, client SDKs, MCP, and a
control-plane UI remain later work.

The direct CLI path runs caller-owned prompts without workflow policy. Built-in workflows cover
coding to a verified pull-request Proposal and repository-grounded codebase investigation. See
[Getting started](docs/getting-started.md) for supported deployment, profiles, commands, and inputs.
To hand installation or operation to an agent, point it at the concise
[Agent guide](docs/agent-guide.md).
The stable remote contract and deployment boundary are in the
[Remote Control API reference](docs/control-api.md).

## Build

```bash
mise trust --yes
mise install --locked
mise run build
.dorf/bin/dorf version
```

## Development

Development requires Mise and Docker Compose. Dorf and its checks run natively; Compose supplies the
disposable PostgreSQL dependency:

```bash
docker compose -f compose.dev.yaml up --detach --wait postgres
mise install --locked
mise run db:init
mise run check
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the complete repository contract.
Architecture and authority details are indexed in [docs/README.md](docs/README.md).
