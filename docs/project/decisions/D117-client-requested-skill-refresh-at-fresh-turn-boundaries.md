# D117: Clients request skill refresh at fresh Turn boundaries

- **Applicability:** current
- **Areas:** client-api, harnesses, persistence
- **Read when:** Changing skill discovery after client-managed Sandbox updates.
- **Decision history:** Accepted, 2026-09-11.
- **Decision:** Retain an optional refresh request on Message admission. Existing delivery selection
  determines when it can run. A Steer defers refresh to a fresh Turn in the same Agent lane.
- **Why:** Clients know when installed skills change. Dorf knows when native execution can safely
  refresh discovery. Reusing Message and Turn history preserves that separation without another
  refresh operation or busy-state record.
- **Boundary:** The original request remains immutable. Effective refresh derives from requested
  Follows since the last accepted fresh Turn and Steers targeting that Turn. Native acceptance
  proves the requested reload preceded submission; pre-submission failure leaves it pending.
  Clients continue to own safe file installation and activation.
- **Proof:** PostgreSQL tests cover replay, restart, a queued Follow preceding a refresh Steer,
  failure retention, and subsequent ordinary Turns. Native tests cover requested and omitted
  reloads, definite failure, and accepted-turn recovery. Unsupported profiles reject admission.
