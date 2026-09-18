<picture>
  <source media="(prefers-reduced-motion: reduce)" srcset="assets/cover.png">
  <img alt="Agents continuing Sessions inside isolated Sandboxes in the Dorf village" src="assets/cover.gif">
</picture>

<p align="center"><strong>Your agents. Your infrastructure. One API.</strong></p>

# Dorf

**Dorf is the open-source control plane for running agent harnesses on infrastructure you control.**

Dorf's direction is to carry a supported agent setup into compatible isolated infrastructure
without rebuilding it in a new agent framework. Dorf provides a stable API and manages Session
configuration, compute, access, supported recovery, and requested cleanup. The native harness owns
the conversation and execution. The [North Star](docs/project/north-star.md) describes this accepted
direction; the current Message API is being simplified in the [Session slices](docs/implementation/session-product-proposals.md).

```text
Deployment-host CLI      Remote clients
       |                       |
  loopback HTTP          HTTPS ingress
       +-----------+-----------+
                   |
             Dorf deployment
       durable execution and recovery
                   |
       Sandbox providers x Harnesses
```

Dorf is a stateful, self-hosted control plane, not an agent framework or an embeddable runtime SDK.
An enrolled CLI admits a direct Session and operates its interaction
loop—Messages, observation, eligible recovery, exact Sandbox files,
cleanup, and bounded Session listing—through one configured Dorf deployment. The deployment-host CLI
uses fixed authenticated loopback HTTP; remote clients use operator-owned HTTPS ingress. Each
deployment publishes its OpenAPI and typed Problem catalog. Static release manifests define
separately supervised API and worker services; one resumable `dorf setup` flow prepares their
protected configuration and applies that exact Compose project. Operators use Compose directly only
for advanced lifecycle operations. Client SDKs, MCP, and a control-plane UI remain later work.

Clients supply instructions, prepare their workspace, and decide what the results mean and when to
release resources. Coding, review, and publication policy belong to those clients. See
[Getting started](docs/getting-started.md) for supported deployment, profiles, commands, and inputs.
To hand installation or operation to an agent, point it at the concise
[Agent guide](docs/agent-guide.md).
The stable remote contract and deployment boundary are in the
[Remote Control API reference](docs/control-api.md).

## Build and contribute

Follow [CONTRIBUTING.md](CONTRIBUTING.md) for the repository-managed setup, build, and verification
contract.
Architecture and authority details are indexed in [docs/README.md](docs/README.md).
