# Earn its keep

Source: Fred Brooks, [No Silver Bullet](https://en.wikipedia.org/wiki/No_Silver_Bullet) (essential vs
accidental complexity).

Accidental complexity is the main enemy. Every line, file, option, layer, doc and test must earn its
place in the outcome.

- Delete before you add. Prefer the smallest change that solves the real problem.
- No speculative guards, flags, parameters or edge-case handling for cases that have not happened.
- LOC is a consequence, not the goal: fewer competing representations, not fewer characters. A
  "simplification" that adds net LOC needs a reason.
- Prefer changes that compound: name the recurring burden removed or the next work made cheaper.
  The easiest change is not automatically the most valuable one.

Test: If this were gone, what would the user lose, and is that worth what it costs to understand and
maintain?
