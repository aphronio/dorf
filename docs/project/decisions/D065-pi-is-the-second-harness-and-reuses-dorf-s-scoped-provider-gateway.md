# D065: Pi is the second Harness and reuses Dorf's scoped Provider Gateway

- **Applicability:** current
- **Areas:** harnesses, model-access, sandboxes
- **Read when:** Changing Pi integration, native session recovery, or scoped Provider Gateway custody.
- **Decision history:** Accepted; second-Harness coding-to-PR terminal proven; image packaging refined by D066 — 2026-08-13
- **Decision:** Pi, distributed as `@earendil-works/pi-coding-agent`, is the deliberately selected
  second Harness for the D063 portability sequence. Its Incus profile uses the shared Debian 13
  image while runtime selection starts Pi rather than Codex. A Sandbox-resident Pi RPC process owns the live native session;
  the native Pi session maps to a Dorf Thread, each accepted RPC prompt starts a Dorf Turn, and Pi's
  queued steering user entries remain within their target Dorf Turn. The RPC process survives
  host-side Worker loss and exposes explicit prompt acceptance, settlement, follow-up, and steering
  operations. Profile selection is a startup choice; common workflow and consumer code remain
  Harness-independent.
- **Connection custody:** Existing named AI connections, including the owner's ChatGPT
  subscription connection, remain under Dorf's Provider Gateway. The Pi Sandbox receives only the
  same Job- and Sandbox-scoped route credential used by Codex profiles and addresses that route as an
  OpenAI Responses provider. Dorf does not copy Pi or ChatGPT OAuth bundles into the image or
  Sandbox.
- **Proof boundary:** The accepted evidence currently covers image construction, route creation, one
  clean initial no-change AgentRun, native Thread and Turn observation, and SIGKILL recovery after
  submission but before durable binding with exactly one native user Turn, followed by route
  revocation and Sandbox cleanup. One follow-up Message is also proven to append exactly one native
  user Turn to the same Pi session. The resident RPC transport is proven for one clean initial
  no-change AgentRun with an explicit successful prompt response and final `agent_settled` event.
  Controller loss after an accepted RPC prompt is proven to recover the same native Turn without a
  duplicate prompt. Active-Turn steering is proven to wake the durable workflow, acknowledge the
  exact target Turn, persist its queued native user entry inside that logical Turn, settle both the
  target and steer AgentRuns, and retain an unchanged exact-Revision observation before route and
  Sandbox cleanup. Isolated review is proven on a committed exact Revision: deterministic policy
  selected one general Role, its dedicated Sandbox ran Pi with only `read`, `grep`, `find`, and `ls`,
  the checkout remained clean and immutable, review-observation Evidence was retained, and ordinary
  feedback returned to and settled through the implementation Thread. The coding-to-PR terminal is
  proven by a Pi implementation commit, exact-Revision Checks, that isolated review loop, an
  unchanged feedback follow-up, scoped branch push, and exact open GitHub Proposal. Pi is the first
  verified second Harness for the current Incus coding workflow profile.
- **Why:** Pi's documented RPC mode is its headless JSON protocol for embedding from a non-TypeScript
  control plane. It supports the required OpenAI Responses transport and lets Dorf test Harness
  portability without adding a TypeScript SDK sidecar or another provider-custody system. D066 owns
  the later decision to package both credential-free Harness executables in one image.
- **Reconsider when:** Pi cannot preserve no-duplicate recovery or required intervention semantics,
  its native session format cannot remain authoritative for observation, or a smaller supported
  integration proves the portability boundary with less profile-specific machinery.
