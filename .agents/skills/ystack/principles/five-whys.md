# Five whys

Source: Toyota Production System (Sakichi Toyoda, Taiichi Ohno).

Ask why until you reach a need you cannot remove or a cause you can fix. Five is a habit, not a rule.

- For a requirement: why do we need it, who asked, what happens without it. Stop at the person's real
  outcome. Existing code, tradition and "we might need it" are not owners.
- For a bug: why did it fail, why was that possible, why did nothing catch it. Fix the deepest cause
  you can reach in scope; patching the first symptom stacks cures on a fragile design.
- For a proposed fix: why this fix, why not delete the thing that needs fixing.
- When two fixes with the same premise have failed, stop fixing and question the premise.

Write the chain in one line each. If a link is a guess, label it and test it.

```text
report is late -> job waits for a lock -> lock held by the nightly export -> export rereads everything
               -> root: export has no incremental path, so delete the full reread
```

Test: Is this the real need or cause, or just the first one I saw?
