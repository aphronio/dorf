<picture>
  <source media="(prefers-reduced-motion: reduce)" srcset="assets/cover.png">
  <img alt="Agents continuing Jobs inside isolated Sandboxes in the Dorf village" src="assets/cover.gif">
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
CLI can admit a direct Job or a documented built-in workflow and operate their common interaction
loop—Messages, observation, eligible recovery, exact Sandbox files, verified Evidence metadata,
cleanup, and bounded Job listing—through one configured Dorf deployment. The deployment-host CLI
uses fixed authenticated loopback HTTP; remote clients use operator-owned HTTPS ingress. Each
deployment publishes its OpenAPI and typed Problem catalog. Static release manifests define
separately supervised API and worker services; one resumable `dorf setup` flow prepares their
protected configuration and applies that exact Compose project. Operators use Compose directly only
for advanced lifecycle operations. Generic workflow registration, client SDKs, MCP, and a
control-plane UI remain later work.

The direct CLI path runs caller-owned prompts without workflow policy. Built-in workflows include
coding to a verified pull-request Proposal and repository-grounded codebase investigation. See
[Getting started](docs/getting-started.md) for supported deployment, profiles, commands, and inputs.
To hand installation or operation to an agent, point it at the concise
[Agent guide](docs/agent-guide.md).
The stable remote contract and deployment boundary are in the
[Remote Control API reference](docs/control-api.md).

## Build and contribute

Follow [CONTRIBUTING.md](CONTRIBUTING.md) for the repository-managed setup, build, and verification
contract.
Architecture and authority details are indexed in [docs/README.md](docs/README.md).
