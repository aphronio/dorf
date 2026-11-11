# Slice playbook

New or changed behavior, built as one vertical slice.

1. Stage 1: state the user outcome in one sentence. Run five whys on each requirement. Name owner and
   evidence. Ask the owner only about product calls you cannot settle by experiment.
2. Read the owning docs and code for the area. Find the native path in the tools already in use.
3. Stage 2: list what can be deleted before building (dead code, old paths, options, docs). Delete it
   first and verify.
4. Name the data shape and its owner before writing logic. Show it as a short ASCII sketch if the
   change crosses a boundary.
5. Build the thinnest end-to-end loop. One lever at a time. Fast loop first, then the full run.
6. Prove it on the real surface. Check for regressions. Label evidence.
7. Stage 3 pass: was this the simplest way? Remove what the slice made unnecessary.
8. Update only docs whose claims changed. Record a durable decision in the decision log.
9. Commit your own changes atomically. Push only if asked.
10. Close the loop (SKILL.md section 6).

Reply: what the user can do now, evidence, LOC delta, decisions made, limits, Needs you.
