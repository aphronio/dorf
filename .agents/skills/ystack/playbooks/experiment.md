# Experiment playbook

Tune, benchmark or compare: configs, engines, models, prompts, queries, build settings. The
deliverable is a measured decision and a record.

1. Restate the goal as one metric to move, the quality it must not lose, and the real workload it
   serves.
2. Build the quality gate before any speed run: fixed inputs scored against an exact or trusted
   reference. Measure the noise floor by rerunning the reference with a harmless change. A change is
   lossless only if it stays inside that floor.
3. List the levers from the tool's own flags, docs and changelog. Tag each lossless (same output) or
   lossy (changes output).
4. Fast loop: one lever per arm, interleaved arms (A B B A), two or more rounds, deterministic
   inputs, warm-up excluded. Keep every raw row.
5. Explain each outlier from logs before trusting or dropping it: caches, contention, background
   work.
6. Confirm the winners on the real workload. Synthetic gains often shrink there.
7. Apply lossless wins yourself. For lossy ones, show the speed/quality frontier and let the owner
   pick.
8. Verify on the surface the owner uses. Re-run the gate when the tool updates; defaults move.
9. Finish this run before the next. A stage is done when it is measured against ground truth, not
   when it runs. Before any bigger run, rerun the changed pipeline on the last run's inputs: it must
   reproduce the known results. Prefer a check in code that refuses the bigger run until it does.
10. Record it in a dated `experiments/` folder: goal, method, every arm including rejected ones, raw
   data, scripts and the choice. A default that wins is a result too.

Rules for every experiment:

- Each threshold records what it rests on: measured, or a guess. Gate on the outcome the owner cares
  about; proxies, guesses and confidence wishes only rank. Never gate on guesses multiplied together.
- Compare like with like: same windows, same scales, same units across sources.
- Before a batch, run one item and check its output (and, for a paid call, the billed price). Make
  reruns free: cache answers, save progress, resume. Stop when the need is met, not when the source
  runs out.
- When the subject is a set of sources (providers, models, people), show their disagreement as a
  range. Their measured disagreement is the noise floor for any claim built on them.

Domain recipes add the specifics. Open the one that matches:

- [models and inference](experiment-models.md): local models, quants, inference engines, sampling,
  AI benchmarks.

Reply: before/after table, the frontier as a picture, what was rejected and why, Needs you for lossy
picks.
