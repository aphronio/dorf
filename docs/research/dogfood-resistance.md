# Active Dorf dogfood resistance

This non-normative backlog contains only unresolved resistance observed while operating Dorf against
Dorf. Resolved items are removed: accepted boundaries belong in the
[Decision Log](../project/decisions.md), while regression tests own executable proof.

## Resistance to discuss

### P1 — Workflow-specific network failures arrive late

The broad profile probe verifies provider lifecycle, the baseline file operation, required tools,
Harness startup, and cleanup. It deliberately cannot prove every repository clone, dependency
redirect, or task-specific endpoint a future Job may need. A host forwarding failure therefore
consumed all bounded task attempts during early investigation dogfood.

Discuss the smallest workflow-owned readiness or failure-classification improvement that shortens
this feedback loop without turning profiles into repository dependency manifests, silently changing
host firewall policy, or adding a fine-grained provider capability matrix.

### P2 — Empty capability lists serialize as `null`

Admission reports `required_provider_capabilities: null` when a workflow requires none.

Discuss normalizing collection-shaped public output to `[]` while leaving the internal absence of
optional capabilities semantically unchanged.

### P2 — Visible client coordination repeats deployment details

A followable interactive run still requires coordinating setup, profile, Provider Connection, Job,
Worker, and observer commands. This is valid Core composition but repetitive client work.

Discuss a thin client or skill driven by workflow input/output contracts. Keep presentation,
Slack/GitHub tags, terminal layout, and notification policy outside Dorf Core.

## Deliberate non-goals

Do not treat this backlog as justification to:

- install every repository tool in the generic Sandbox artifact;
- add a provider registry or fine-grained capability matrix;
- make investigation run arbitrary repository setup or tests by default;
- move interaction-channel policy into Dorf Core;
- generalize explicit workflows merely to reduce line count;
- silently change host firewall, network, or provider configuration; or
- move Harness custody out of the Sandbox before a concrete use case earns that boundary.

## Suggested discussion order

Re-rank only from new dogfood evidence:

1. workflow-specific network failure feedback;
2. stable empty-collection JSON; and
3. thin interaction clients outside Core.
