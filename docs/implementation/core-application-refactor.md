# Core Application Refactor: Outstanding Proof

Status: temporary implementation aid. The accepted application boundary is authoritative in the
[North Star](../project/north-star.md#product-boundary),
[Architecture](../project/architecture.md#execution-model), and Decision Log
[D088](../project/decisions.md#d088--core-is-a-small-in-process-custody-contract-organized-by-job-ownership)
through [D092](../project/decisions.md#d092--investigation-reports-remain-sandbox-files).
Do not restate those contracts here. Delete this file when the proof below is complete.

## Unfinished proof

- Earn a direct trusted-client slice that admits complete Core Job intent without workflow identity
  and drives the same Job, Sandbox, Agent, exact-file-read, and requested-cleanup path as native
  workflows. The concrete client must bring honest durable scheduling and claim recovery. If it is
  external, transport and authentication must be proved with it; do not manufacture a public API,
  SDK, registry, or workflow abstraction merely to close this item.
- Dogfood the refactored application path through a current verified E2B profile. The evidence must
  cover exact Job-owned Sandbox use, initial Message and same-Thread follow, active steer without a
  duplicate Turn, caller-known file read before cleanup, controller or executor-loss recovery, and
  confirmed route and Sandbox absence after explicit cleanup. Across current dogfood terminals,
  exercise both supported Harnesses through profile selection without provider or Harness branches
  in workflow code.

Record any consequential correction in the Decision Log. Otherwise keep the proof as operational
evidence and delete this temporary file rather than extending it with completed narration.
