# Dorf documentation

## Use Dorf

| Intent | Read |
| --- | --- |
| Install and run the existing Python implementation | [Getting started](getting-started.md) |
| Diagnose setup, host, Incus, Codex, or provider trouble | [Setup support](support.md) |

Command syntax comes from the installed CLI. Run `dorf --help`, `dorf worker --help`, or
`dorf job --help` rather than treating design examples as an API reference.

## Understand and contribute

| Intent | Read |
| --- | --- |
| Understand the accepted product direction | [North Star](project/north-star.md) and [Principles](project/principles.md) |
| Understand the selected Go and Absurd design | [Greenfield Architecture](project/architecture.md) |
| Read the superseded Python boundary as behavioral evidence | [Legacy Runtime Surface](project/runtime.md) |
| Contribute code | [Contributing](../CONTRIBUTING.md) |
| Report a vulnerability | [Security policy](../SECURITY.md) |
| Publish a release | [Release process](releasing.md) |

## Document types

### Project

[`project/`](project/) contains accepted direction, principles, responsibility boundaries, and
decisions. These documents guide product and architecture work; they are not CLI tutorials or
compatibility promises.

- [North Star](project/north-star.md) — approved experience and product direction; aspirational
- [Principles](project/principles.md) — enduring product and engineering judgment
- [Greenfield Architecture](project/architecture.md) — accepted Go, Absurd, and PostgreSQL boundaries
- [Runtime Surface](project/runtime.md) — superseded Python/SQLite boundary retained as evidence
- [Showcase Ideals](project/showcase-ideals.md) — workflow-layer direction kept outside the runtime
- [Provider Gateway](project/provider-gateway.md) — provider connection and scoped-route boundary
- [Decision Log](project/decisions.md) — accepted choices and reconsideration triggers
- [Orchestration](project/orchestration.md) — maintainer protocol for running implementation epics

### Implementation

[`implementation/`](implementation/) contains maintainer handoffs, current implementation evidence,
and end-to-end validation records. It explains why and how the present slices were built, but it is
not a stable end-user contract.

- [Core setup](implementation/core-setup.md)
- [Incus image](implementation/incus-image.md)
- [Buzz deployment](implementation/buzz.md)

### Research

[`research/`](research/) is archival and non-normative. It records ecosystem investigations and
historical inputs. It is not a source of Dorf requirements or current support claims.
