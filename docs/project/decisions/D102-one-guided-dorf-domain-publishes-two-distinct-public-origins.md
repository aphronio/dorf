# D102: One guided Dorf domain publishes two distinct public origins

- **Applicability:** current
- **Areas:** deployment, model-access, client-api
- **Read when:** Changing guided Cloudflare ingress or either public Dorf origin.
- **Decision history:** Accepted from remote-client dogfood — 2026-08-27; live public Job proof passed —
  2026-08-27
- **Decision:** Guided Cloudflare setup asks for one Dorf domain, leaves its apex untouched, and
  suggests editable direct-child hostnames `api.DOMAIN` for the Control API and `models.DOMAIN` for
  the Model Gateway. It reconciles one named outbound-only Tunnel with the exact selected pair and
  persists that pair for unchanged replay. The CLI prints and connects to the Control API origin;
  Sandbox Profiles retain the separate model URL. The origins share Tunnel custody, not protocol or
  application authority.
- **Boundary:** This is one setup-owned Cloudflare convenience, not an ingress registry or general
  proxy. Fresh unused hostnames require no additional confirmation; replacing unrelated resolving
  DNS requires explicit operator consent. Any custom Control API origin remains operator-owned and
  reaches host port `8745`. Any custom Provider Gateway route remains explicit deployment input;
  advanced `--gateway-url` changes only that route, never infers Control API ingress, and is not
  mixed with retained guided Tunnel state. The retired single-host Tunnel state is rejected before
  host mutation rather than carried as a migration path.
- **Why:** Remote dogfood proved that preparing only the Sandbox model route left the intended Dorf
  deployment unusable by a remote Client. Separate direct children preserve the apex for a future
  site and avoid relying on nested-host certificate coverage. The guided Tunnel is already a
  setup-owned foreground Compose service, so two fixed host routes complete the zero-friction setup
  without adding another supervisor, credential, or ingress abstraction.
- **Public Job proof:** An independently enrolled workstation Client authenticated through the
  public Control API origin and admitted a direct Job against the verified remote Incus Profile.
  The Job completed a real `gpt-5.6-sol` Turn, returned an exact 33-byte workspace file through the
  public API, and reached cleanup `complete`. The same Client then received `file_unavailable` for
  that Sandbox, proving that the public cleanup fence had closed.
- **Refines:** D036's named Tunnel, D097's Control API deployment, D100's operator-owned-ingress
  posture, and D101's Compose topology. The detailed operator flow lives only in
  [Getting started](../../getting-started.md#1-install-the-application-initialize-a-deployment-host);
  runtime contracts live in the [Provider Gateway](../provider-gateway.md) and
  [Remote Control API](../../control-api.md#deployment-services).
- **Reconsider when:** Another guided ingress provider earns equal end-to-end proof, one hostname
  must serve several Deployments, or a hosted topology needs independent lifecycle and identity.
