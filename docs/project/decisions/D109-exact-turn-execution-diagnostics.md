# D109: Export diagnostics through the existing native Turn binding

- **Applicability:** current
- **Areas:** core, harnesses
- **Read when:** Changing native execution diagnostics, attribution, or exporter custody.
- **Decision history:** Accepted, 2026-09-10.
- **Decision:** Let the Codex adapter observe selected app-server notifications and export them
  through an optional host-owned OpenTelemetry logs provider. Attribute each event using the
  existing Message and AgentRun plus its acknowledged or durably bound native Turn.
- **Why:** Native OTLP logs do not consistently carry a Turn or propagated trace identity. In the
  pinned runtime, nested code-mode tools can lose their parent's trace and usage can have no trace.
  A conversation ID cannot distinguish successive messages. The native notification protocol
  supplies exact Thread and Turn IDs without a runtime fork or inferred matching.
- **Ownership:** Core retains execution authority; the adapter owns native protocol interpretation;
  the host exporter owns diagnostic delivery. A client joins its own Run to the existing Message
  ID. No new persisted correlation field, public native identity, or diagnostic ledger is added.
- **Limits:** Export and subscriptions are best effort. Completed history can reconstruct different
  item IDs and omit tool events, so it is not replayed as live events. Steer is not a separate model
  run. The observer does not supply complete model context, usage accounting, or every native tool.
- **Proof:** Concurrent WebSocket tests isolate identical prompts by exact acknowledged Turns;
  a reconnect test observes only its durably bound Turn. The paired Agent0 proof used real admission,
  HTTP, PostgreSQL, the durable worker, and the pinned Codex binary against controlled model and
  provisioning fixtures, then queried the resulting records in Logfire.
- **Reconsider when:** The native exporter provides complete explicit per-Turn attribution, or a
  demonstrated diagnostic requirement justifies durable export or richer native event coverage.
