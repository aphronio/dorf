# D139: Codex routes use native launch overrides

- **Applicability:** current
- **Areas:** harnesses, model-access
- **Read when:** Changing Codex route installation, native configuration ownership, or app-server launch.
- **Decision history:** Separates model-route settings from client native configuration.
- **Decision:** Install Dorf-owned route options and a scoped credential separately. Pass options
  as native app-server configuration overrides without editing the client configuration file.
- **Why:** Codex already composes launch overrides with native configuration. Reusing this mechanism
  eliminates a TOML merge layer, marked-block editing, and a compare-and-replace writer for files
  Dorf does not own. Route precedence is explicit rather than a client-configuration conflict.
- **Compatibility:** Existing processes keep their launch configuration. Older workspaces without
  the new route-options file keep using native provider settings until a subsequent installation.
  Never delete or attempt to repair legacy client configuration during route removal.
- **Verification:** An opt-in local test runs stock Codex through the production launch script,
  reads effective configuration, reconnects, and restarts with a different route. It verifies native
  MCP and preference preservation and byte-identical client configuration. Adapter tests cover
  installation, replay, removal, and literal argument transport. This is not a live inference or
  checkpoint-recovery proof.
- **Authority:** [Provider Gateway](../provider-gateway.md) owns route configuration and custody semantics.
