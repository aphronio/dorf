# D106: Remove unused workflow capability declarations

- **Applicability:** current
- **Areas:** workflows, sandboxes
- **Read when:** Adding optional provider requirements or workflow runtime metadata.
- **Decision history:** Replaces D069's optional provider-capability declarations, 2026-09-05.
- **Decision:** Remove the optional capability matcher and workflow-definition objects. Runtime
  composition carries the selected Sandbox profile name directly. Workflow constants own identity
  and revision; ordinary functions describe current work. Investigation consumes Git workspace
  execution directly without a forwarding service object.
- **Why:** No shipped workflow requires an optional provider capability, and no provider supplies
  one. The only nonempty requirement belonged to a hypothetical browser-workflow test. The
  declaration objects wrapped constants, while the runtime object wrapped a profile name and an
  always-empty list. These layers added concepts without enforcing a real requirement.
- **Preserved behavior:** Verified-profile admission, exact runtime profile identity, workflow
  revision checks, remote Git prerequisites, and AgentRun authority remain enforced. The Control
  API, including the investigation and cleanup contract used by Agent0, is unchanged. No durable
  records or task identities change.
- **Reconsider when:** A shipped workflow requires a concrete operation beyond the baseline Sandbox
  and Harness contracts. Define and prove that operation before introducing shared declarations.
