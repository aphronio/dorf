# Solved elsewhere

Most problems were solved before, in another field or another decade. Search there before inventing.

Games are a favorite source: tight frame, memory and network budgets made them solve early what apps
meet later. One authoritative world state with save/load and replay, fixed-step time and queued
commands, server authority with client prediction, per-frame budgets.

Other fields: manufacturing (Toyota: five whys, stop the line, kanban), aviation and medicine
(checklists, blameless incident review), finance (double-entry ledgers, idempotent transfers),
distributed systems (event logs, idempotency keys), and history (how institutions handled the same
coordination problem without software).

How to use it:

1. Name the problem in plain, field-neutral words ("two writers, one document, unreliable network").
2. Ask which field lives with that problem every day, and find its standard answer.
3. Read the source. Take the idea, not the machinery: a game technique is not a mandate for a game
   engine.

Pointers: Robert Nystrom, [Game Programming Patterns](https://gameprogrammingpatterns.com/);
Glenn Fiedler, [Gaffer On Games](https://gafferongames.com/) (networking, fixed timestep);
[1500 Archers on a 28.8](https://www.gamedeveloper.com/programming/1500-archers-on-a-28-8-network-programming-in-age-of-empires-and-beyond)
(lockstep); [Factorio Friday Facts](https://factorio.com/blog/) (determinism, optimization);
Atul Gawande, The Checklist Manifesto.

Test: Who has had this exact problem for decades, and what did they settle on?
