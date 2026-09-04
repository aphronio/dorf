# D100: Automation contract and managed host services stay narrow

- **Applicability:** partial
- **Areas:** client-api, deployment
- **Read when:** Changing the automation API contract, Client administration, or service lifecycle.
- **Decision history:** Automation contract accepted and dogfooded; service lifecycle superseded by D101 —
  2026-08-26
- **Decision:** Publish one embedded OpenAPI 3.1 document for the exact remote surface and derive the
  runtime RFC 9457 Problem responses and its `x-dorf-problems` catalog from one central authority.
  Add newest-first keyset listing of bounded Job summaries, stable JSON authentication status, and
  host-only Client list, show, and idempotent revoke commands. Do not add a generator dependency,
  SDK family, remote Client administration, or another status or event model.
- **Service lifecycle:** Superseded by D101's static setup-applied Compose lifecycle. D100's
  operator-owned custom HTTPS ingress remains accepted; D102 adds one guided Cloudflare exception.
  Dorf-specific status, restart, logs, reconciliation, update handoff, systemd units, notification,
  fragment custody, and journal facts are historical rather than current interfaces.
- **Historical proof:** The full live PostgreSQL suite passed. A non-TTY direct HTTPS client derived
  the Job-list operation from the published OpenAPI document, traversed a page, and received the
  catalogued `invalid_cursor` response for an altered cursor. A temporary Client appeared in host
  list and show,
  an idempotent revoke returned the same revoked identity, and the next request received `401`. A
  clean managed-service install passed `systemd-analyze verify`; a real Job completed across a worker
  restart, remained available across an API restart, and cleaned up successfully. Both units then
  remained enabled, current, and ready.
- **Why:** Automation needs one discoverable schema, bounded enumeration, stable machine failures,
  and a supported process-loss boundary. Keeping Client administration host-local and custom
  ingress operator-owned completes those needs without inventing OAuth, roles, deployment contexts,
  an SDK ecosystem, or a general ingress product.
- **Refines:** D097's authentication and deployment split, D098's common remote interaction, and
  D099's closed Job union. The shipped boundary is documented in the
  [Remote Control API](../../control-api.md).
- **Reconsider when:** A second concrete client earns generated distribution artifacts or a real
  organization needs identity federation or roles. Deployment-lifecycle reconsideration belongs to
  D101; guided ingress belongs to D102.
