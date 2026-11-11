# One owner

Every fact, type, rule, definition and diagram has one authoritative home. Everything else points to it.

- Generate derived types from the owning schema (backend schema to client types, models to enums)
  instead of hand-copying. Drift starts with the second copy.
- Do not store duplicated state. Derive projections from the owner.
- Code owns exact behavior, API shapes, defaults and commands. Docs own intent, rationale,
  non-obvious constraints and evidence. Prose or comments that restate code rot; comment only a
  non-obvious why.
- Decisions go in the decision log with the reason and when to reconsider. Slice detail and session
  notes do not belong in long-lived docs.
- When you find a copy, delete it and link the owner. Do not keep the two in sync.
- Reveal detail only when needed: start readers (and agents) with the smallest useful map and link
  down to detail, so nobody loads history to learn today's rule.

Test: Which file owns this fact, and why does it also appear here?
