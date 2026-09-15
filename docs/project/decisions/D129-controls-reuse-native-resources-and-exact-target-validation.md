# D129: Controls reuse native resources and exact target validation

- **Applicability:** current
- **Areas:** core, interaction
- **Read when:** Changing Steer connection ownership or exact native interruption recovery.
- **Decision history:** Extended bounded reuse to Steer and removed the redundant native pre-interrupt read, 2026-09-15.
- **Decision:** Steer may reuse the existing operation scope across required history, durable baseline,
  submission, and recovery. The Codex Stop adapter sends the exact native interrupt and reads its
  outcome. It does not first reconstruct that same Turn from history.
- **Why:** The native interrupt validates the requested Thread and active Turn identity. Wrong, old,
  and foreign Turn requests do not interrupt a successor. This validation belongs to the native
  mutation boundary. Steer's prior history still establishes whether its input was already accepted.
- **Recovery:** Native rejection does not establish the bound Turn's outcome. Exact history does.
  Transport uncertainty requires a fresh authenticated read and never replays the interrupt within
  the operation. A still-active or missing target does not become a successful Stop by inference.
- **Ownership:** Core retains its effect fence and durable transitions. Reuse ends with the callback;
  it does not create a cross-claim cache, new observation lifetime, or Sandbox activity lease.
- **Proof:** Native contract experiments cover active, terminal, missing, old-with-successor, and
  foreign targets. Adapter tests cover call counts, exact binding, cancellation, native rejection,
  and lost acknowledgements. These proofs exclude model latency.
