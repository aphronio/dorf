# D010: Vertical slices, KISS, and deletion are preferred

- **Applicability:** current
- **Areas:** product, release
- **Read when:** Changing implementation through a migration or deciding whether compatibility code is required.
- **Decision history:** Accepted — 2026-07-22
- **Decision:** Migrate through thin end-to-end slices. Current code and tests may be refactored or
  removed when a simpler implementation replaces them.
- **Why:** Preserving accidental structure creates adapters around adapters. Tests protect required
  product behavior, not superseded implementation shape.
- **Reconsider when:** An explicit compatibility promise covers the affected interface.
