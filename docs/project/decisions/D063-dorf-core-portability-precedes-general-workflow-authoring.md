# D063: Dorf Core portability precedes general workflow authoring

- **Applicability:** current
- **Areas:** product, core, client-api
- **Read when:** Changing Core portability priorities, profile admission, or the boundary for workflow and client composition.
- **Decision history:** Accepted product direction — 2026-08-13; client composition refined by D088 —
  2026-08-21; narrow external projection added by D097 and expanded by D098 and D099 — 2026-08-26
- **Positioning:** Dorf is the open-source control plane for running agent Harnesses on infrastructure
  its owner controls: your agents, your infrastructure, one API. Core is the product. Whole-setup
  portability is direction; Codex and Pi with local Incus coding-to-PR on the supported host are the
  current verified Harness claims. D065 records the completed second-Harness proof.
- **Profile contract:** A verified profile covers the supported Harness version and configuration,
  skills, extensions or plugins, project instructions, workspace image or setup and dependencies,
  vendor-supported credential or subscription connection, host constraints, tools, isolation,
  recovery, interruption, and observation. Connection custody never implies copying raw user secrets
  into a Sandbox; scoped routing or injection remains adapter- and profile-specific.
- **Authority:** Current Core, workflow, and client ownership is defined only by the
  [North Star product boundary](../north-star.md#product-boundary) and corrected by D075. A Harness
  remains authoritative for its native session, transcript, and tool protocol.
- **Composition:** Native workflows and trusted client adapters are Core dogfood and compose the
  same small ownership boundary without a privileged hidden path. D088 defines the in-process
  Job/Sandbox/Agent composition; D097 through D099 add the deliberately narrower authenticated HTTPS
  projection for direct Jobs and two fixed typed workflows. SDK and generic public workflow
  direction remain uncommitted.
  Dynamic agent-authored recipes remain a later UX layer; Dorf is not a generic automation canvas,
  graph framework, agent builder, or model/tool Harness.
- **Proof order:** Starting from Codex on Incus, D065 proves Pi as the second supported Harness on
  Incus. Next prove Codex on a second Sandbox provider, then cross Pi and that provider. The
  mechanical oracle is
  that common consumer and workflow code has no Harness- or Sandbox-specific branches beyond profile
  selection and capability admission.
- **Supersedes and refines:** Supersedes D062's research-workflow-first proof order. It also refines
  older second-workflow extraction gates, including D009, D047, and D061: a later workflow still adds
  its natural facts before common authoring seams are extracted, but workflow generality is not the
  next product proof. D088 permits direct trusted-client composition in-process, while D097 owns the
  first narrow external projection; neither authorizes an SDK, generic workflow API, provider
  matrix, plugin system, or marketplace.
- **Reconsider when:** A supported Harness cannot fit the AgentRun boundary, a second Sandbox cannot
  preserve the Job authority model, or real external-client use shows that profile selection and
  capability admission do not keep common code independent of Harness and Sandbox.
