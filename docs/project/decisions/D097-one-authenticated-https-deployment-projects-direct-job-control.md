# D097: One authenticated HTTPS Deployment projects direct Job control

- **Applicability:** current
- **Areas:** client-api, deployment
- **Read when:** Changing authenticated remote Job control or its deployment trust boundary.
- **Decision history:** Accepted and dogfooded — 2026-08-26; projection expanded by D098 and D099; automation
  contract refined by D100, deployment lifecycle refined by D101, and guided ingress refined by
  D102
- **Decision:** Expose one deliberately narrow external client boundary for a configured Dorf
  Deployment over HTTPS. The initial surface admits direct Jobs, returns their situation-first
  projection, and accepts an explicit cleanup request; D098 and D099 expand that same boundary. It
  does not expose Core as a generic network API or add a client SDK, MCP, a control-plane UI, or
  named multi-deployment contexts.
- **Authentication:** The Deployment has one `deployment-operator` Principal. A host operator creates
  a short-lived, one-use Enrollment; redeeming it registers one Client with its own client-generated
  opaque credential. Dorf retains only credential digests, and each Client identity can be revoked
  independently. This is a single-operator trust boundary, not multi-user identity, roles, or
  organization membership.
- **Deployment:** Compose maps the API service's HTTP listener directly to host port `8745` for
  custom operator-owned HTTPS ingress and joins it to D102's optional guided Cloudflare ingress. It
  authenticates clients, admits and projects Jobs, and requests cleanup, but it registers no
  execution handlers. A separate durable worker owns task execution and recovery. D100 later proved
  their independent process-loss boundary; D101 now supervises both in one static Compose project
  applied by the continuous setup flow.
- **Why:** SSH grants host authority that ordinary Job control neither needs nor should imply, while
  exposing every Core mechanism would be premature. The smaller authenticated projection makes a
  self-hosted deployment useful off-host without granting database, executor, provider, or host
  access and preserves the in-process Core ownership boundary.
- **Refines:** D063's client composition, D088's in-process-only external posture, and D095's original
  local direct CLI. The current transport contract is the
  [Remote Control API](../../control-api.md); its staged design and proof record is
  [archived](../../history/control-api-slices.md).
- **Reconsider when:** A second real client earns a language SDK or the single-operator Principal
  cannot represent actual users.
