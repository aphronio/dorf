# D153: Remote checkpoints use bounded worker guards

- **Applicability:** historical
- **Areas:** client-api, core, sandboxes
- **Read when:** Changing remote checkpoint capture, publication confirmation, worker credential custody, or branch control.
- **Decision history:** Accepted 2026-09-24; extends D152's operator branch surface to authenticated clients. Superseded by D154 on 2026-09-24.
- **Decision:** The Control API exposes fixed capture and branch operations through the existing private worker boundary. Provider and checkpoint-storage credentials remain worker-owned. Clients need no deployment shell, provider credentials, callback URL or remote executable.
- **Capture:** A short-lived worker attempt runs the existing guarded capture. After immutable upload, the client observes the exact provisional reference, pins its own state, then confirms. The original native guard remains active until validation and checkpoint publication. Native activity, timeout and cancellation invalidate the attempt. A provisional reference is never sufficient evidence of publication.
- **Lifetime:** Pending guards are process-local, bounded and deliberately not resumable. Worker replacement loses them; clients discard unconfirmed application artifacts. Brief terminal observations let a client reconcile a lost confirmation response. Successful checkpoint facts and branch requests retain their existing durable owners. No second workflow ledger or application snapshot enters Core.
- **Branch:** Stable request identity, exact checkpoint selection, held preparation, verified release and source independence retain D152's semantics. Local operator commands and the public API share the branch implementation.
- **Why:** Remote clients can compose coherent application/native saves without host authority. A bounded guard is sufficient because interruption can safely abandon a provisional save; persisting a lease or reproducing a native watcher after restart would add machinery without preserving the actual guard.
- **Scope:** One managed worker owns pending capture attempts. This does not promise resumable captures across workers or every-turn autosaves. The [checkpoint contract](../../implementation/session-checkpoints.md) owns limits and verification status; [OpenAPI](../../../internal/controlapi/openapi.json) owns the wire contract.
