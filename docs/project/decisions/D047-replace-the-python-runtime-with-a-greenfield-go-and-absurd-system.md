# D047: Replace the Python runtime with a greenfield Go and Absurd system

- **Applicability:** partial
- **Areas:** core, persistence, workflows
- **Read when:** Changing the Go, PostgreSQL, or Absurd foundation and its ownership boundaries.
- **Decision history:** Accepted foundation — 2026-08-06; review-request and Absurd-usage clauses superseded
  by D048 — 2026-08-09; commit ownership clarified by D050 — 2026-08-10; review handoff and unknown
  review selection superseded by D052 — 2026-08-10; harness execution vocabulary clarified by
  D055 — 2026-08-10
- **Decision:** Replace the current Python and SQLite implementation with a Go application using
  Absurd on PostgreSQL for durable execution. Dorf-owned PostgreSQL tables retain product facts;
  Absurd owns task claims, checkpoints, retries, waits, and wake events. Keep external-effect
  reconciliation in Dorf because no workflow engine can atomically commit an Incus, agent, Git, or
  GitHub effect together with its own checkpoint. Incus remains the first Sandbox, Codex app-server
  the first agent Harness, and GitHub pull requests the first acceptance surface.
- **Product vocabulary:** A coding request is a `Job`; its isolated execution body is a `Sandbox`;
  a bounded invocation of an agent in a named `Role` is an `AgentRun`. A `Harness` hosts agents, a
  `Thread` is its continuing conversation context, and a
  `Turn` is one request/response cycle. `Action`, `Check`, `Revision`, and `Evidence` name
  deterministic work and proof. Do not
  create a durable `Worker` identity until a real product requirement needs personality or memory
  across Jobs. Do not introduce `Assignment` or `RoleRun` as aliases for facts that these names
  already express.
- **Review policy (superseded by D048):** Start with a pure deterministic classifier over observed
  change facts. Mandatory rules select security, browser, performance, or other bounded review Roles
  when their explicit conditions match; documentation-only changes with green Checks may select
  none. Only an unknown classification may invoke one bounded semantic triage AgentRun.
  Implementation prose is not a policy input, and there is no optional-request mechanism. The
  durable Job coordinates mechanics, so there is no default Coordinator Agent and no
  review-after-every-change ritual.
- **Replacement strategy:** Build vertical slices on a `greenfield` integration branch. Each slice
  must reach the smallest real Incus, Codex, repository, and GitHub terminal it claims, then delete
  the Python component and implementation-coupled tests it replaces. The old implementation is
  behavioral evidence, not an API, schema, CLI, packaging, or document-format compatibility target.
  There are no users or old data to preserve: discard SQLite state, do not migrate or dual-write it,
  and do not add a Python facade.
- **Portability boundary:** Do not design a generic durable-engine interface. Localize Absurd task
  sequencing while keeping domain facts, policy, stable Action identities, and external
  reconciliation independent. If Absurd is outgrown, let short-lived active Jobs drain and schedule
  new Jobs on the replacement; completed Dorf domain records remain readable without treating
  Absurd checkpoint history as a portable format.
- **Polyglot boundary:** Go owns the core. Add a language-specific executor only when a concrete SDK,
  such as a TypeScript Sandbox provider or Python environment provider, makes that boundary
  materially smaller. It consumes a dedicated queue through a small versioned contract and may not
  leak vendor types into Job semantics. Do not add a plugin registry or second language in advance.
- **Why Go and Absurd:** Go supplies a small deployment artifact, explicit concurrency and process
  control, strong standard-library coverage, and readable deterministic policy around the Incus,
  agent, Git, and GitHub boundaries. Absurd supplies the durable queue, checkpoint, retry, sleep,
  event, and recovery machinery that Dorf should not reimplement, while retaining a local feedback
  loop and inspectable PostgreSQL authority. Temporal provides a broader and more operationally
  mature platform but adds a server, SDK, event-history, and deployment model beyond the current
  single-product need. Restate provides a polished durable-object and service model but makes that
  runtime a larger architectural center and is less aligned with the chosen fully local,
  application-owned feedback loop.
- **Supersedes:** D014's SQLite choice; D019's Python package topology; D025's mandatory durable
  Worker, Room, and Assignment model; D026's Python/SQLite composition; D034's in-process Python SDK
  boundary; D045's public Python execution facade; and D046's mandatory fixed DeepSeek review cycle.
  D011's one-task/one-branch/one-proposal shape, D012's Incus choice, D013's GitHub acceptance
  boundary, and D015's control-plane-owned GitHub authentication remain useful product constraints,
  expressed through the new vocabulary.
- **Rejected alternatives:** Do not rewrite the Python runtime in place behind compatibility
  adapters, start from an empty repository that loses working operational evidence, adopt Dagger for
  ordinary repository commands, or build a custom database scheduler and recovery controller.
  Dagger may be reconsidered for a repository that already owns a Dagger contract or needs one
  reproducible cross-host build graph; direct repo-owned commands are simpler for the first system.
- **Reconsider when:** Absurd cannot survive the real crash and redelivery terminals, its evolution
  or license makes self-hosting unsuitable, multi-region or high-volume hosted operation requires a
  more mature distributed control plane, a second concrete language or agent Harness proves a
  smaller stable seam, a real cross-Job identity requirement earns `Worker`, or repeated dogfood
  identifies a concrete authoritative input that deterministic facts plus bounded triage cannot
  express and that justifies a structured additional-review contract rather than parsing agent
  prose.
