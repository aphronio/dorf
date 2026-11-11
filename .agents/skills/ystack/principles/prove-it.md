# Prove it

Output is a claim. An observation, a real run or an external authority establishes what happened.

- Verify on the real artifact and surface, used as a normal user would: the running app, the real
  device, the deployed service, the actual document. A proxy (it compiles, tests pass, the evaluator
  scored it, the API accepted it) is not the effect.
- Look at the source yourself: the image, the OCR, the log, the screen. Do not trust a deterministic
  evaluator over your own review when ground truth is incomplete.
- Keep evidence categories apart: simulator vs hardware, local vs deployed, fixture vs live model,
  accepted vs delivered vs seen.
- Check for regressions in what you did not mean to change.
- Scope verification to the change: a small change gets a small, direct check.
- After being wrong once in a task, re-check every claim that depended on the mistake.
- Before trusting a speed or cost number, rule out that you measured something else.
- "X does not exist" needs a search where X would live (the platform, registry or site), not a
  grep of what you already have.
- Separate the builder from the judge: whoever made a change cannot be the only one who evaluates,
  approves or activates it.

Tests:

- Add one when it gives evidence for the current change or prevents a regression you actually hit.
  Prefer a few end-to-end tests through the real path over unit tests that mirror implementation.
- Delete brittle tests: constants, internal wiring, exact copy, model choices, absence of deleted code.
- Never weaken a test to make it pass. When a failure reveals a design flaw, fix the design.
- Run expensive suites (evals, full e2e) once on a frozen head, not on every edit.

Test: Which independent observation would prove or disprove this, and did I make it?
