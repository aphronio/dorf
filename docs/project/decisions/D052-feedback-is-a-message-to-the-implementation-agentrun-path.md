# D052: Feedback is a Message to the implementation AgentRun path

- **Applicability:** current
- **Areas:** workflows, harnesses, persistence
- **Read when:** Changing how human, check, workflow, or reviewer feedback reaches AgentRuns.
- **Decision history:** Accepted workflow simplification — 2026-08-10
- **Decision:** User text, Check output, ReviewPolicy prompts, and reviewer text use the same durable
  `Message` primitive. Every AgentRun consumes exactly one Message. ReviewPolicy creates the stable
  workflow Message consumed by a selected review AgentRun; reviewer output is an ordinary agent
  Message consumed through the implementation AgentRun path. Dorf does not parse a universal
  `ReviewResult`, classify findings, or create separate respond-to-review, respond-to-check, or
  repair AgentRun types.
- **Message identity:** A Message records text, Job-local sequence, `FromKind`, and `FromID`.
  `FromKind` names the sender: human, agent, or workflow. `FromID` retains the exact request, AgentRun,
  or Check that caused the Message, and the consuming AgentRun points back to the Message. A Check or
  observation does not send a Message; the workflow turns its result into one. `Feedback` is a use of
  Message, not another stored type. Do not add reply or thread fields until an outward-response
  feature needs them.
- **Review selection:** Deterministic policy selects known specialist Roles. Unknown risk selects one
  bounded general read-only reviewer rather than a triage AgentRun whose prose must be parsed to route
  more work. Role, Revision, and capability envelope specialize an AgentRun; its prompt is the input
  Message. The adapter supplies its Sandbox workspace without persisting that path on the run. These
  inputs do not create a new execution primitive.
- **Outcome:** The implementation agent decides whether to act, ignore, or explain. Dorf observes the
  resulting Git state. A committed descendant becomes a new Revision and loops through Checks and
  policy. A clean unchanged checkout means the Message was handled without a code change. A failed
  mandatory Check still blocks readiness until it passes, but it reaches the agent through the same
  Message mechanism.
- **Authority:** Reviewer prose is an advisory Message, not Evidence. The Message is the durable
  handoff between agent Threads. One observed Evidence record proves the review AgentRun's harness
  execution; the prose is not duplicated as Evidence. Git remains authoritative for commits,
  and deterministic Checks remain authoritative for their results.
- **Why:** This matches ordinary human collaboration: one worker receives feedback from different
  people and tools, decides what it means, and either changes the work or explains why not. One
  AgentRun mechanism, one inbox, and opaque text remove parsers and review-specific state without
  weakening deterministic gates.
- **Reconsider when:** A concrete acceptance surface requires a machine-readable reviewer decision, or
  dogfood proves that ordinary text cannot safely carry a specific cross-agent contract.
