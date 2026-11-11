# Building blocks

Sources: Mitchell Hashimoto, [The Building Block Economy](https://mitchellh.com/writing/building-block-economy)
and [libghostty](https://mitchellh.com/writing/libghostty-is-coming); the Unix philosophy.

- Keep the core small and general. Put workflow-specific policy (approvals, git, one customer's
  rules) in a layer on top, never in core primitives.
- A good block does one job, has few dependencies and a clean interface designed for its callers,
  not a dump of its internals.
- Do not bolt onto a bad building block. Fix or replace the block.
- Get the primitives right first; the rest composes from them. A variant (approve one) can be a case
  of the primitive (approve all).
- Model the domain invariant first. Keep channel- and platform-specific parsing and UX at the edge.
- Thin native shells over a shared core: business rules live in one place; each platform adds only
  presentation and integration.
- Extract a shared abstraction only for current reuse, never for a hypothetical second caller. Do
  not extract an SDK before a second real workflow exists.
- Keep cohesive logic together. Do not split code just to redistribute lines.

Test: Could another workflow use this block without inheriting policy that is not theirs?
