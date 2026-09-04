# D083: Git and coding behavior are workflow modules, not Core

- **Applicability:** partial
- **Areas:** core, workflows, sandboxes
- **Read when:** Changing ownership of Git workspace or coding behavior across Core and workflow modules.
- **Decision history:** Refined by D084, D085, and D086 — 2026-08-20
- **Decision:** Keep Core limited to the existing Job, Message, Sandbox, AgentRun, Action,
  Evidence, recovery, exact Sandbox file reads, and requested-cleanup custody described by the North Star. Place
  exact Git checkout and Revision observation in `internal/gitworkspace`. Place review execution and
  proposal-facing Action kinds in the coding workflow module.
  Sandbox providers expose only their provider-neutral Sandbox contract; they do not implement Git
  clone policy. Shared use by multiple workflows does not make a behavior Core.
- **Composition:** Native coding and investigation workflows statically compose the Git workspace
  module over Core execution. `coding-to-proposal` additionally composes its coding service and
  GitHub authorities. Incus and E2B remain interchangeable Sandbox adapters beneath the same
  provider-neutral boundary.
- **Why:** The first two workflows both used Git, so horizontal reuse had been mistaken for Core
  ownership. Non-repository workflows must be able to consume Core without receiving Git or coding
  authority. Provider adapters should translate infrastructure, not contain workflow semantics.
- **No plugin system:** Ordinary Go package composition is sufficient for the currently compiled
  native workflows. Dynamic discovery, loading, trust, compatibility, distribution, and upgrade
  contracts remain unearned and are not introduced by this correction.
- **Vocabulary:** This decision relocates existing behavior and constants only. It introduces no
  product term and changes no meaning in the North Star vocabulary.
- **Proof:** Core declares no Git workspace interface; the Sandbox contract contains no Git
  operation; one Git workspace implementation is exercised through a provider-neutral Sandbox fake;
  coding execution lives outside the shared custody package; Incus, E2B, native-workflow, CLI, and
  PostgreSQL tests retain the same observable behavior.
- **Reconsider when:** Independently distributed workflow modules require dynamic loading, or a
  non-workflow consumer proves a smaller reusable repository boundary.
