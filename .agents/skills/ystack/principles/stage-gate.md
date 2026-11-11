# Stage gate

Source: Elon Musk's five-step algorithm ([Starbase interview](https://everydayastronaut.com/starbase-tour-and-interview-with-elon-musk/)).
His own warning: he once ran the steps backwards and automated a process that should have been deleted.

Run the steps in order, one stage per pass. Name the stage before you act.

| Stage | Allowed work | Not yet | Exit check |
| --- | --- | --- | --- |
| 1 Question | Ask why until the real need appears ([five-whys](five-whys.md)). Name who owns each requirement and what evidence makes it necessary. | Editing code | Each requirement has an owner and evidence, or is marked for deletion |
| 2 Delete | Remove requirements, features, steps, checks, files, options. Verify the outcome still holds. | Optimizing what survives | Deletion verified on the real path; anything restored has an observed reason |
| 3 Simplify | Fewer concepts and owners in what remains. Use the platform's built-in path. | Speed work, automation | Same behavior, fewer concepts |
| 4 Speed up | Shorten feedback and execution loops. | Automation | Measured before and after |
| 5 Automate | Turn the proven, simplified process into a script, check or tool ([zero-token](zero-token.md)). | | It runs without a human or a model in the loop |

Always start at stage 1 and never skip a stage. What you judge is where to stop.

- Stop at the last stage the work's maturity supports. A prototype whose shape still moves usually
  stops after stage 3: speeding up or automating something that changes next week is waste. A
  stable, repeated process can go on to stages 4 and 5.
- Judge per scope. A small, settled slice inside a young product can go through all five; the
  product as a whole may stop at 2 or 3.
- Say where you stopped and why in one line, so the owner can push you further.

Rules:

- Doing stage 3 work during stage 1 is how agents polish things that should not exist.
- Musk's rule of thumb: if you are not adding back about 10% of what you removed (after checking on
  the real path), you were too timid.
- When a later stage exposes new complexity or a repeated failure, return to stage 1.

Test: Which stage am I in, and is everything I am about to do allowed in it?
