# D019: Portable runtime core with built-in adapters

- **Applicability:** historical
- **Areas:** core, workflows
- **Read when:** Reviewing the replaced Python runtime package and built-in adapter topology.
- **Decision history:** Superseded by D047 — 2026-08-06
- **Decision:** Keep the durable lifecycle and generic persistence in the self-contained
  `dorf.runtime` package. Put concrete implementations in
  `dorf.adapters.agents` and `dorf.adapters.environments`, in the same distribution for now.
  Keep coding-to-PR state and behavior in `dorf.workflows`, outside the runtime and adapters.
  Caller metadata is opaque to the runtime. Do not add registries, plugin loading, provider
  configuration, networking policy, or a separate distribution yet.
- **Why:** Durable Worker, Room, Job, Assignment, and conversation bindings are useful beyond
  the current application and should be inexpensive to extract into another monorepo package or
  repository. In-package adapters make the implementation seams and future extension points clear
  without preserving the deleted single-implementation registries or creating a speculative plugin
  framework. Keeping Git and GitHub behavior in the coding workflow lets a future environment
  adapter reuse the lifecycle without inheriting coding policy.
- **Compatibility:** The prior experimental SQLite schema and top-level implementation modules are
  replaced rather than migrated. No external compatibility promise covered them.
- **Reconsider when:** The runtime is deliberately published or extracted; a second real adapter
  proves a common selection/configuration seam; or packaging adapters separately solves a concrete
  dependency or release problem.
