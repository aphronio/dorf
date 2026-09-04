# D048: Simplify the post-cutover core around Absurd and explicit workflow semantics

- **Applicability:** partial
- **Areas:** core, persistence, workflows
- **Read when:** Changing durable workflow sequencing, message ordering, or Dorf's persisted execution facts.
- **Decision history:** Accepted audit correction; steer fallback superseded by D096 — 2026-08-25
- **Durable sequencing:** Use Absurd's public task, named-step, event, cancellation, and inspection
  surfaces for generic execution mechanics. Dorf retains Job and Revision facts, deterministic
  policy, stable external Action identity, scope, settlement state, and reconciliation because those are product
  semantics or cross-system uncertainty. Production behavior and authority must not query or mutate
  Absurd's raw internal tables or mirror its checkpoints, retries, leases, task state, or recovery
  controller into Dorf-owned schema. Version-pinned white-box tests and operator diagnostics may
  inspect those tables without making them product authority.
- **Message order:** Every accepted message keeps a monotonic Job-local admission sequence. Follow-up
  Turns are FIFO. A `steer` is an explicit priority lane for the active harness Turn and may overtake
  queued follow-ups. D096 preserves that priority intent but supersedes any terminal-target fallback:
  steer remains bound to its exact active Turn and never creates a new Turn. Default text and
  structured inspection, command help, and the admission
  acknowledgement must expose its intent, target, original sequence, and priority effect; an
  architecture document alone is not adequate observability.
- **Review composition (superseded by D052):** Deterministic policy supplies the mandatory Role floor. An implementation
  AgentRun may additionally make a structured, bounded request for an allowlisted Role and optional
  focus. The request cannot remove mandatory review, change capabilities, grant authority, create a
  Role, or authorize recursive or unbounded work. Each selected Role receives its own disposable
  Sandbox and scoped provider route, including read-only review, and those live resources are
  reclaimed after the Role's Evidence is retained.
- **Greenfield schema:** Before the first release, replace the prototype migration chain with one
  clean baseline schema; there is no data or upgrade path to preserve. Defer `sqlc` until after the
  Absurd realignment and schema squash. Generated type-safe query wrappers can remove scan
  boilerplate, but they do not simplify the state model, perform migrations, or replace behavioral
  PostgreSQL tests. Reconsider only if substantial repetitive query plumbing remains in the smaller
  store.
- **Verification:** Agents run deterministic unit and PostgreSQL integration tests locally before
  pushing relevant changes. CI repeats those portable suites as an independent merge gate. Real
  Incus, Codex, provider, and GitHub terminals remain targeted dogfood for changes to those
  authorities rather than simulated requirements for every CI run.
- **Cutover correction:** The final acceptance proposal remains blocked until the audited functional
  gaps are closed: private-repository clone authority, exact initial-turn identity, lease-safe
  mutation ownership, human-requested same-Job revisions, explicit pre-proposal terminal outcome,
  and prompt reviewer-resource cleanup. The correction must preserve the proven exact-Revision,
  Action-reconciliation, publication, outcome, and zero-ghost properties while materially reducing
  the application-owned durable state machine.
- **Why:** The greenfield implementation proved the complete shape, but the proof also exposed that
  Dorf rebuilt generic durable mechanics beside Absurd and accumulated prototype schema and
  implementation narration. Returning those mechanics to the chosen library leaves Dorf's code
  focused on product facts and unavoidable external-system boundaries. Explicit steer and review
  request semantics preserve useful intentional behavior without hiding it behind an inaccurate
  FIFO or prose-policy description.
- **Reconsider when:** Absurd's public APIs cannot express a proven recovery terminal without losing
  required evidence, measured reviewer isolation cost justifies a different model, or the smaller
  post-realignment store still contains enough repetitive typed query plumbing for `sqlc` to remove
  more code than its generator and generated surface add.
