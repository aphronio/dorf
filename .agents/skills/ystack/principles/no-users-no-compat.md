# No users, no compat

While a product is a prototype with no external users and no legacy data, the past has no vote.

- No backward compatibility, migrations for old data, deprecation periods, compat aliases or
  rollback paths. Migrate the callers and delete the old path in the same change.
- No fallbacks. Fail fast and loud so the root cause surfaces. A fallback hides the error you need
  to see.
- No defensive code when you own both sides of a boundary. Trust your own types.
- Big rewrites are fine when the result is smaller and clearer. Preserve learning, not
  implementation: user memory and corrections, usage, cost and outcome history, eval cases and
  results. Delete caches, rebuildable indexes and obsolete schemas.
- Real external boundaries (third-party APIs, shipped clients, client work in someone else's repo)
  are different: there, make minimal changes and keep compatibility.

Check before applying: does this project have real users or data that must survive? If unsure, ask
once and record the answer in the project's AGENTS.md.

Test: Who exactly would this compatibility path protect today?
