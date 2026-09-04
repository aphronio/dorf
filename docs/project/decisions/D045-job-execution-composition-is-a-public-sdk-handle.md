# D045: Job execution composition is a public SDK handle

- **Applicability:** historical
- **Areas:** core, client-api, workflows
- **Read when:** Reviewing the former public SDK handle for composing Job execution dependencies.
- **Decision history:** Superseded by D047 — 2026-08-06
- **Decision:** `Dorf.job_execution(JOB)` is the one public composition point for a recorded Job's
  runtime, Room environment, Codex driver, repository command execution, provider-route command
  rewriting, and Git-credential refresh. The coding workflow consumes that bound handle and keeps
  GitHub, Git, repository-contract, and coding-store policy; it does not construct runtime or
  adapter implementations. Disposable admission uses the same facade-owned environment and driver
  composition without creating a second workflow abstraction.
- **Why:** Coding setup, admission, active workflow commands, and detached input delivery had each
  reconstructed overlapping pieces of the same Job execution stack. Their concrete repetition
  earns this seam, while a bound handle remains smaller than a registry, plugin system, or workflow
  base class and leaves runtime semantics unchanged.
- **Reconsider when:** A second real environment or agent implementation proves the concrete facade
  needs selection policy, or a non-coding workflow demonstrates a smaller shared operation than the
  bound Job handle.
