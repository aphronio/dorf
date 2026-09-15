# D130: Exact file reads have source and transfer bounds

- **Applicability:** current
- **Areas:** core, interaction
- **Read when:** Changing Sandbox file capture, HTTP file transport, or cleanup protection.
- **Decision history:** Replaced unlimited exact reads with explicit size and transfer bounds, 2026-09-15.
- **Decision:** Bound exact file capture at `sandbox.MaxFileReadBytes`, using the opened descriptor
  and reading at most one extra byte to detect overflow. Return a typed oversized-file error without
  partial contents. Public HTTP uses the stable `file_too_large` Problem with status 409.
- **Why:** Arbitrary or growing files must not determine unbounded source transfer and response
  allocation. A prior size check alone cannot establish the bound. Streaming has additional
  lifecycle and slow-consumer costs that the current exact-file contract does not require.
- **Ownership:** Each HTTP handler has its own `sandbox.MaxConcurrentFileReads` budget, acquired
  before source access and held through response delivery. Public and private handlers must not
  share one semaphore: a public read can call the private reader while holding its own slot.
  The existing Job fence protects capture and is released before slow response delivery.
- **Boundaries:** HTTP clients limit materialization even for missing or incorrect length headers,
  then preserve digest and length validation. Direct Go callers own retained bytes. The bounded
  source command does not establish a general bound against malicious provider RPC output.
- **Proof:** Tests cover exact limits, overflow, growing descriptors, path rules, malformed
  responses, typed errors, queued cancellation, slow consumers, and control access while file
  transfers exhaust their separate budget.
