# Bug playbook

1. Reproduce on the same surface the owner saw it. No reproduction, no fix; say what you tried.
2. Five whys to the root cause. Write the chain. Label guessed links and test them.
3. Stage 2 first: can the failing part be deleted or the fragile design removed instead of patched?
4. Fix the cause, not the symptom. No fallback that hides it. Fail fast if the state is impossible.
5. Prove the fix on the original reproduction and check nearby behavior for regressions.
6. Add a regression test only if it would catch a realistic recurrence.
7. If the same class of bug can recur, encode a check (see encode-lessons).
8. Commit atomically. Close the loop.

Reply: the cause chain, the fix, the evidence, LOC delta, what else could break the same way.
