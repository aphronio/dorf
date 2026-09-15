# D128: Execution changes wake the attached task

- **Applicability:** current
- **Areas:** core, persistence, interaction
- **Read when:** Changing execution wake hints, native completion delivery, or durable wait ordering.
- **Decision history:** Accepted monotonic execution wake hints, 2026-09-15.
- **Decision:** Message admission, Stop, and exact native completion signal the Job's existing
  Absurd task. Each reconciliation pass captures a wake revision before reading authoritative
  execution facts, then waits for the next revision. The existing bounded timeout covers a lost
  signal or an unavailable native observer. Core advances at most one Message per pass. The consumer
  rechecks its policy before advancing another eligible Message.
- **Ownership:** Dorf and the Harness retain their existing execution facts. Absurd alone decides
  task eligibility, claims, cancellation, and recovery. Wake revisions select immutable event names
  and carry no permission to submit, interrupt, settle, or clean up work.
- **Ordering:** A dedicated PostgreSQL row serializes wake revisions. Recording a new cause,
  advancing its revision, and emitting the corresponding Absurd event commit together. Repeated
  causes reuse their event. The wake transaction does not acquire the external-effect fence.
- **Why:** A native completion and a Stop request can arrive while a task waits for another
  Message. Reusing that Message's immutable event would make later waits resolve repeatedly.
  A fresh revision covers all wake sources and prevents an early notification from causing a busy
  loop when native history still reports an active Turn.
- **Tradeoff:** Wake metadata adds bounded coordination records per unique cause. It avoids a new
  worker, process-local execution owner, or dependency fork for multi-event waits. Retained FIFO
  Message events support older workers during a rolling deployment. Older producers remain covered
  by the existing timeout.
- **Proof:** Local verification uses PostgreSQL, the real Absurd worker, and a controllable Harness.
  It covers early and repeated signals, timeout recovery, stale observations, transaction rollback,
  and sleeping tasks releasing worker capacity. Measured latency excludes model inference.
