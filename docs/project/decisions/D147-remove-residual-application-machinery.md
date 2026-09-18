# D147: Remove residual application machinery

- **Applicability:** partial
- **Areas:** persistence, workflows, client-api
- **Read when:** Changing Session execution facts, scheduling, or retained application machinery.
- **Decision history:** Completes D144's removal of application policy after D146's Session rename,
  2026-09-18. D148 supersedes durable input custody as product direction;
  the existing Message implementation remains during the coordinated native API transition — 2026-09-18.
- **Decision:** Remove Session workflow identity and AgentRun role, capability, input revision,
  and the unused strict-review submission nonce. Remove the workflow sender kind. Keep native
  acceptance attribution and resource ownership. Name the retained failure fields ExecutionAttention.
- **Simplification:** One Session execution path needs no role envelope or application prompt
  resolver. Delete the unconsumed FIFO wake, unused named-Sandbox and Core file-reading helpers,
  ordinary task-scheduling wrapper, pre-atomic cleanup polling loop, obsolete application fixtures,
  and unused Git revision API schema. Default provisioning uses the admission reservation.
- **Preservation:** A new append-only migration removes unused columns, preserves the listing index,
  and renames failure-attention fields in place. Retain Messages, native Thread/Turn attribution,
  resource generations, effect fences, cleanup scheduling, retry budgets and queued task identities.
  Previously published migrations remain unchanged. No retired application records need conversion.
- **Deployment:** Stop old API and worker processes before applying the new migration and restarting
  the matching version. There are no dual reads, old API aliases, or background compatibility loops.
  Applying code checks to disposable databases does not deploy this change.
- **Verification:** PostgreSQL proofs retain direct Session input, queued delivery, native binding,
  resource custody, failure attention and migration replay. Existing concurrency, delivery recovery,
  checkpoint, interruption, file access and cleanup tests exercise the active paths. Tests dedicated
  to removed application behavior are deleted. Native provider protocols are unchanged.
- **Authority:** [North Star](../north-star.md#product-boundary),
  [Architecture](../architecture.md#execution-model),
  [cleanup tracker](../../implementation/session-product-proposals.md#post-session-cleanup-audit).
