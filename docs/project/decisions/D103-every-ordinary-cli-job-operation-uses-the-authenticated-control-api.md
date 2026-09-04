# D103: Every ordinary CLI Job operation uses the authenticated control API

- **Applicability:** current
- **Areas:** client-api, interaction, deployment
- **Read when:** Changing how CLI Job operations reach deployment authority.
- **Decision history:** Accepted implementation direction — 2026-08-27
- **Decision:** The deployment-host CLI and remote CLI use the same authenticated control API for
  Job admission, listing, inspection, watch, Messages, retry, Evidence, exact Sandbox file reads,
  coding abandonment, and cleanup. Host administration remains local. The CLI does not reopen
  PostgreSQL, Core, workflow services, Absurd, or the blob store for an ordinary Job operation.
- **Host Client:** Setup enrolls an ordinary Client through the existing Enrollment contract and
  stores its proof in protected host state. PostgreSQL `control_clients` remains the only
  authentication authority. The proof has no role, scope, bypass, or second token table. A saved
  remote `client.json` takes precedence. Otherwise Job commands use the fixed
  `http://127.0.0.1:8745` origin. The loopback client disables proxies, DNS, alternate destinations,
  and redirects. Compose publishes plain HTTP only on `127.0.0.1:8745`.
- **Setup recovery:** Setup saves a candidate credential and Enrollment before redemption. A rerun
  reuses an authenticated proof, replays an interrupted redemption, or rotates an invalid proof.
  Setup reports ready only after authenticated `GET /v1/me` succeeds. Revocation takes effect
  immediately, and a later setup run enrolls a replacement.
- **Surface:** Direct, coding, and investigation admissions accept an optional named AI connection.
  Model is optional at both the CLI and API. The Deployment resolves an omission from the selected
  AI connection before admission, while an explicit model overrides it for that Job. Every accepted
  Job durably pins the exact resolved connection and model, and replay uses those pinned values. The
  CLI also rejects an invalid Sandbox file path with workspace-relative correction before sending
  it, while the API retains the same exact-read validation for direct callers.
  `PUT /v1/jobs/{job}/abandon` records the coding workflow's idempotent `abandoned` Outcome and then
  requests cleanup. The worker's private reader adds one exact stored-Proposal observation so the
  API receives no GitHub credential or generic GitHub proxy.
- **Deletion:** Remove the top-level local `inspect`, `message`, `retry`, `evidence`, `abandon`, and
  `cleanup` commands and their distinct output models. Keep the `dorf job` vocabulary and canonical
  snapshot watch. Do not replace the deleted local history renderer with an event API. Delete local
  source admission, storage, restoration, and tests. Do not add host roles, source upload, API blob
  writes, or a private route.
- **API failure:** A stopped or unhealthy control API is deployment-service work. Job commands fail
  with setup, doctor, and Compose repair guidance. They never use direct storage as a break-glass
  mutation path.
- **Why:** Two adapters for the same Job operations had different flags, projections, recovery, and
  tests. Preserving every host-only presentation feature would replace that duplication with host
  authorization, upload, and history contracts. One narrow transport deletes the split authority
  while retaining guided setup, Cloudflare, provider custody, Profiles, diagnostics, and Compose
  repair.
- **Supersedes and refines:** Supersedes D095's in-process CLI adapter and D073's local-source
  admission. Refines D060's and D068's CLI names and D099's source-admission boundary. Refines D097
  through D101 by using their existing authentication, typed workflow, automation, capability, and
  Compose boundaries on the deployment host. There are no users or admitted Jobs that require
  migration or recovery support for the deleted source path.
- **Proof:** The control-path audit reports zero local Job dispatch cases and more than 1,000 net
  authored lines removed. Tests cover host credential replay, remote precedence, loopback credential
  containment, all three named-connection admissions, abandonment replay, Job listing,
  PostgreSQL integration, and the published OpenAPI contract.
- **Reconsider when:** A real client earns durable event history, a typed bounded source-upload
  contract, or a separate host role whose authority cannot be expressed by an ordinary Client.
