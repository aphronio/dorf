# D090: Open Jobs may have no current workflow operation

- **Applicability:** current
- **Areas:** core, workflows, harnesses
- **Read when:** Changing how an open Job waits when no workflow operation is eligible.
- **Decision history:** Accepted execution correction; message admission refined by D096 — 2026-08-25
- **Decision:** When an open Job's authoritative facts expose no eligible workflow operation, keep
  its existing attached Absurd task durably waiting. Do not project a workflow `WaitForInput`
  operation, persist an idle phase or status, complete the task, or attach another task for a later
  Message. A follow accepted while admission is open wakes that same execution owner, including when
  it entered the FIFO before an earlier Turn settled; the internal AgentRun reuses the authoritative
  retained Harness Thread and owns its distinct Turn.
- **Recovery:** The Message and AgentRun transaction remains authority and its deterministic event is
  only a wake hint. The task reloads durable facts on a bounded timeout when a hint is lost, and a
  replacement executor reclaims the same attachment after process loss. Cleanup still closes
  admission under the Job fence, cancels the ordinary task, and attaches only the cleanup handoff.
- **Ownership:** Absurd continues to own waits, wakes, claims, retries, and cancellation. Dorf owns
  Message facts, task attachment, and deterministic wake emission. Workflows retain Outcome,
  attention, report-path, and typed execution-envelope policy; an absence of current work adds no
  Core or workflow vocabulary.
- **Refines:** D075's same-Thread investigation revision loop, D087's workflow task composition, and
  D088's requirement that task attachment and wake details remain behind the Core application
  behavior. It preserves D048's FIFO/steer rules and D068's operator retry authority.
- **Proof:** PostgreSQL integration reaches a completed investigation AgentRun, observes the one
  attached task remain nonterminal with no current workflow operation, constructs a fresh executor
  client, admits a follow through `AgentHandle`, and completes a distinct Turn on the same Thread
  without another task attachment. Explicit cleanup reconciles route revocation before Sandbox
  deletion; `REPORT.md` remains readable only before that request.
- **Reconsider when:** Absurd cannot preserve a durable open wait or bounded lost-wake recovery, or a
  real client requires terminal execution ownership while still accepting later input. Either case
  must preserve one Message-to-AgentRun identity and must not silently introduce another scheduler.
