# Zero token

Source: Kelsey Hightower, "ZTA: Zero Token Architecture" (see [giants](../references/giants.md)):
infer once, export, then run without inference. Neither humans nor agents should solve the same
problem twice. This is stage 5 of the [stage gate](stage-gate.md), applied to model work.

- When the same model work repeats (a lookup, transform, check or decision), use the model once to
  produce a script, query, tool or pipeline, then run that artifact without the model.
- Cache expensive results. Validate inputs in code before spending tokens.
- Use the cheapest model that passes for routine work; keep the strongest for judgment.
- Track tokens, cost, time and cache hits when cost matters. Ask before an expensive run.

Test: Will this run again, and could the second run cost zero tokens?
