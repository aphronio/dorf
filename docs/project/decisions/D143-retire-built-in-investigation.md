# D143: Retire built-in investigation

- **Applicability:** current
- **Areas:** workflows, client-api, persistence
- **Read when:** Changing the supported workflow surface or retiring application-owned records.
- **Decision history:** Retires the investigation behavior from D069, D074, D079, D092, and D099,
  2026-09-18.
- **Decision:** Remove the built-in investigation workflow, its runtime, typed admission, report
  projection, CLI/API/client entry points, and source table. Clients drive repository investigation
  through direct Jobs and own repository setup, instructions, report paths, and result meaning.
- **Why:** Direct execution already supplies the required Message and workspace operations.
  Removing application policy reduces maintained code without introducing replacement abstractions.
- **Preserved behavior:** Direct and coding admission, ordered Messages, native conversation
  continuity, file and command access, and cleanup remain supported. Generic Job, Message, and
  resource receipts survive the schema migration; published migrations remain immutable.
- **Retirement:** Complete investigation cleanup and export any needed application source data
  before upgrading. The new migration drops only the investigation source table. There is no
  retained investigation executor or conversion into direct Jobs. Retired Job kinds are excluded
  from public listing and inspection, and the removed admission route returns not found.
- **Verification:** PostgreSQL migration coverage checks retained generic input and resource
  custody and replay. Existing direct execution, coding, and public-boundary tests remain.
  Tests belonging only to the removed workflow are deleted.
- **Authority:** [North Star](../north-star.md#workflow-examples) owns application policy;
  [Control API](../../control-api.md#resources) owns the public contract;
  [getting started](../../getting-started.md) owns upgrade and usage guidance.
