# Dorf documentation

Start here, then open only the document that owns the subject you need. This map also tells
contributors where a documentation change belongs.

Keep each fact, contract, and procedure in one authoritative document. A short projection may link
to that authority for a different reader, but it must not copy exact versions, counts, inventories,
commands, or proof steps. Update a projection only when its own promise changes.

## Find and update the owner

| Authority | Read when | Update only when |
| --- | --- | --- |
| [Public README](../README.md) | You need Dorf's short public promise and newcomer handoff. | The public promise, main supported path, or newcomer handoff changes. |
| [Contributor contract](../CONTRIBUTING.md) | You set up, build, validate, or contribute to the repository. | The development prerequisites, repository-managed workflow, or contribution rules change. Keep exact tool and service configuration in their code-owned files. |
| [Getting started](getting-started.md) | You need installation, setup, or CLI procedures. | A supported command, input, output, sequence, or operator action changes. |
| [Agent guide](agent-guide.md) | A human delegates installation or operation to an agent. | Agent-specific authority, human pauses, secret handling, safety, or handback changes. Do not copy the general procedure here. |
| [Support](support.md) | You need supported combinations, readiness, diagnostics, or fault attribution. | A support claim, prerequisite, readiness meaning, repair, or fault owner changes. |
| [Release process](releasing.md) | You build or publish a release. | Release construction, validation, publication, or installed artifact contents change. |
| [North Star](project/north-star.md) | You need product direction, vocabulary, ownership, or workflow examples. | The product promise, vocabulary, ownership boundary, desired experience, or proof standard changes. |
| [Principles](project/principles.md) | You need repository-wide product or engineering judgment. | The durable judgment itself changes. Do not add feature-specific rules. |
| [Visual style](project/style.md) | You apply Dorf's brand and interface presentation. | Brand assets or presentation rules change. |
| [Architecture](project/architecture.md) | You need durable authority, composition, recovery, or schema-evolution rules. | One of those technical boundaries changes. Concrete package and schema details remain in code. |
| [OpenAPI document](../internal/controlapi/openapi.json) | You need the exact HTTP operation, schema, status, or Problem inventory. | The machine contract changes. Update it with the handlers and tests that implement and verify the contract. |
| [Remote Control API](control-api.md) | You need client-visible HTTP semantics or the managed service boundary. | Authentication, idempotency, observation, operation semantics, or the managed service boundary changes. Do not copy the exact OpenAPI inventory. |
| [Decision guide](project/decisions.md) | You need the rationale for a current or replaced choice. | Never edit the generated guide. Follow the [decision procedure](../CONTRIBUTING.md#record-a-decision). |
| [Provider Gateway](project/provider-gateway.md) | You need model-route ownership or security boundaries. | Provider Gateway responsibility, custody, network, security, or recovery changes. |
| [Buzz deployment](implementation/buzz.md) | You operate the persistent Buzz deployment. | That deployment's infrastructure or operating procedure changes. |

A code change that preserves every documented behavior above needs no documentation edit. A
cross-cutting change may alter several distinct promises. Update each owner, but do not repeat one
changed fact across them. If a fact would need manual edits in several files, keep the exact value
in its owner and replace the other copies with links or stable summaries.

## Archived and non-normative material

Material under `research/` and `history/` is archival and non-normative. Neither directory defines
current Dorf requirements. Open those directories only when a task needs historical evidence,
archived product exploration, or an ecosystem comparison.

These research documents remain useful as narrow entry points:

| Goal | Non-normative research |
| --- | --- |
| Evaluate remote self-managed Sandbox hosts and an outbound connector | [Private provider attachment](research/private-provider-attachment.md) |
| Review the dated provider-evaluation starting point | [Sandbox and VM watchlist](research/sandbox-vm-watchlist.md) |
