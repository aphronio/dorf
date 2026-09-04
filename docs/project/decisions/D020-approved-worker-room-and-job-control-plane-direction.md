# D020: Approved Worker, Room, and Job control-plane direction

- **Applicability:** historical
- **Areas:** product, core, persistence
- **Read when:** Reviewing the former Worker, Room, and Job control-plane direction that preceded D025.
- **Decision history:** Superseded by D025 — 2026-07-27; storage authority refined by D023 — 2026-07-26
- **Decision:** Adopt [north-star.md](../north-star.md) as approved product direction for the L1 control
  plane, including Worker, Room, and Job as product vocabulary and the shared control verbs. `spawn`
  may provision and bind a Worker to a Room before `assign` pins and delivers the Job goal; the
  implementation may represent that interval as an unassigned Job rather than introducing an
  independent worker registry. Aim for a tangible Job document directory alongside transactional
  operational state as refined by D023. Existing session/environment/turn names, SQLite schemas,
  and internal APIs are implementation evidence,
  not compatibility constraints, and may be replaced rather than migrated. Concrete API and schema
  details remain decisions to make in working vertical slices.
- **Why:** The finalized north star is the accepted destination, not a speculative alternative to
  the current implementation. Preserving experimental representations would invert that priority.
  Keeping concrete details refactorable still allows implementation evidence to expose genuine
  conflicts without silently weakening the product direction.
- **Local and remote durability:** A remote or cloud Room should continue when local clients are
  offline. A local Room requires its host to be powered on, but client/process failure or host
  restart must not erase the durable Job, Room, Worker binding, or recovery path.
- **Compatibility:** There is no current external compatibility promise. Rewrite or delete
  superseded runtime code and tests while retaining coverage for behavior the north star still
  requires.
- **Reconsider when:** A working slice exposes a concrete conflict in the accepted vocabulary,
  object lifecycle, directory representation, or control verbs. Discuss and record the deliberate
  revision; do not diverge merely to preserve current implementation shape.
