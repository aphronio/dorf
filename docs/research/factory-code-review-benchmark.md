# Factory Code Review Benchmark — Research Notes

Archived, non-normative snapshot captured on 2026-04-29. Rankings, models, and prices are not
current Dorf guidance.

Captured for designing the dorf review block. All facts here are sourced
from Factory's public benchmark and open-source repos.

## Source

- Blog post: [Which Model Reviews Code Best?](https://factory.ai/news/code-review-benchmark) — Factory Research, Nizar Alrifai, 2026-04-29.
- Open-source benchmark: [droid-code-review-evals/review-droid-benchmark](https://github.com/droid-code-review-evals/review-droid-benchmark)
  (golden set, eval scripts, raw results).
- GitHub Action that runs the same review: [Factory-AI/droid-action](https://github.com/Factory-AI/droid-action).
- Review skill (rubric + output format): [Factory-AI/skills/skills/code-review/SKILL.md](https://github.com/Factory-AI/skills/blob/main/skills/code-review/SKILL.md).
- Per-PR test repos: [droid-sentry](https://github.com/droid-code-review-evals/droid-sentry),
  [droid-grafana](https://github.com/droid-code-review-evals/droid-grafana),
  [droid-keycloak](https://github.com/droid-code-review-evals/droid-keycloak),
  [droid-discourse](https://github.com/droid-code-review-evals/droid-discourse),
  [droid-cal_dot_com](https://github.com/droid-code-review-evals/droid-cal_dot_com).

## Methodology (Factory's setup)

- **13 models** scored against **50 real PRs** drawn from 5 mature OSS repos
  (Sentry, Grafana, Keycloak, Discourse, Cal.com). 145→167 human-validated
  golden bugs (v3 dataset).
- **Identical prompts and identical reasoning effort = `high`** across all 13
  models.
- Each model evaluated **3 times** per PR. F1 = harmonic mean of precision and
  recall vs. the golden set, scored by an LLM judge.
- **Cross-judge validation**: swapping the judge model shifted scores ≤2 pp →
  no home-court bias.
- Pricing: multiplier-based token pricing, apples-to-apples (no hidden
  subsidies / batch discounts).

## How Factory's Review Actually Runs (Two-Pass Pipeline)

```diagram
╭────────────────╮  candidates   ╭───────────────╮  validated  ╭──────────────╮
│ Pass 1:        │──────────────▶│ Pass 2:       │────────────▶│ Inline GH /  │
│ Generate       │  (recall)     │ Validate &    │  (precision)│ GL comments  │
│ candidates     │               │ filter        │             │              │
╰────────────────╯               ╰───────────────╯             ╰──────────────╯
```

- **Pass 1 — Candidates** ([review-candidates-prompt.ts](https://github.com/Factory-AI/droid-action/blob/dev/src/create-prompt/templates/review-candidates-prompt.ts))
  reads diff + PR description + existing comments, loads the `code-review`
  skill, emits `[P0|P1|P2]` findings with file/line + suggestion blocks. Tuned
  for high recall.
- **Pass 2 — Validator** ([review-validator-prompt.ts](https://github.com/Factory-AI/droid-action/blob/dev/src/create-prompt/templates/review-validator-prompt.ts))
  re-reads the full diff plus the candidates, runs the skill's "Pass 2:
  Validation" procedure, drops false positives, fixes anchors, validates
  suggestions. Tuned for precision.
- The judge that scored the benchmark uses `claude-opus-4-6` and matches
  semantically against golden comments (see
  [scripts/eval_common.py](https://github.com/droid-code-review-evals/review-droid-benchmark/blob/main/scripts/eval_common.py)).

## The Review Skill (Rubric)

From [code-review/SKILL.md](https://github.com/Factory-AI/skills/blob/main/skills/code-review/SKILL.md), priority order:

1. **Correctness & Edge Cases** — null/empty/boundary, error handling, sound logic.
2. **API & Behavior Changes** — breaking changes, backwards compat.
3. **Maintainability & Readability** — naming, complexity, duplication.
4. **Tests** — new tests, updates, edge-case coverage.
5. **Performance (when relevant)** — N+1, hot loops; only flag clearly problematic.
6. **Security Basics** — input validation, authz, secrets, sensitive data.

Severity buckets: **Must-fix / Suggestions / Nits**. Output sections: Summary,
Must-Fix, Suggestions, Nits, Verification (test/manual steps).

## Their Picks

| Pick           | Model         | Cost/PR  | F1     | Note                                                          |
|----------------|---------------|----------|--------|----------------------------------------------------------------|
| Best Overall   | GPT-5.2       | $1.25    | 60.5%  | Top-tier quality at half the cost of Opus 4.6.                 |
| Best Value     | Kimi K2.5     | $0.41    | 51.9%  | ~85%+ of top-tier quality at a fraction of the price.          |
| Budget Pick    | MiniMax M2.7  | $0.15    | 45.6%  | 8 review passes for less than one GPT-5.2 run.                 |

Pareto frontier: MiniMax M2.7 → Kimi K2.5 → GPT-5.4 Mini → GPT-5.2.

## Full Rankings

| #  | Model           | Mean F1 | Stdev | Precision | Recall |
|----|-----------------|--------:|------:|----------:|-------:|
| 1  | GPT-5.2         | 60.5%   | ±3.0  | 65.0%     | 57.6%  |
| 2  | Opus 4.6        | 59.8%   | ±2.1  | 58.1%     | 61.8%  |
| 3  | Sonnet 4.6      | 57.4%   | ±4.9  | 62.6%     | 47.3%  |
| 4  | Opus 4.7        | 55.9%   | ±3.2  | 62.1%     | 54.2%  |
| 5  | GLM-5.1         | 55.8%   | ±2.8  | 63.5%     | 50.7%  |
| 6  | GPT-5.3 Codex   | 55.7%   | ±3.1  | 62.7%     | 50.8%  |
| 7  | Gemini 3.1 Pro  | 52.1%   | ±2.4  | 55.4%     | 49.4%  |
| 8  | Kimi K2.5       | 51.9%   | ±1.6  | 71.5%     | 40.7%  |
| 9  | GPT-5.4 Mini    | 51.5%   | ±1.7  | 56.6%     | 48.1%  |
| 10 | Gemini 3 Flash  | 49.5%   | ±2.2  | 60.1%     | 42.8%  |
| 11 | GPT-5.5         | 47.9%   | ±1.9  | 47.5%     | 48.4%  |
| 12 | GPT-5.4         | 47.5%   | ±1.0  | 59.6%     | 41.8%  |
| 13 | MiniMax M2.7    | 45.6%   | ±4.3  | 59.1%     | 43.7%  |

## Cost Efficiency

| Model           | F1     | Cost/PR | $/F1 Point | Tokens/PR |
|-----------------|-------:|--------:|-----------:|----------:|
| MiniMax M2.7    | 45.6%  | $0.15   | $0.003     | 56K       |
| Gemini 3 Flash  | 49.5%  | $0.34   | $0.007     | 124K      |
| Kimi K2.5       | 51.9%  | $0.41   | $0.008     | 152K      |
| GPT-5.4 Mini    | 51.5%  | $0.68   | $0.013     | 252K      |
| GLM-5.1         | 55.8%  | $1.06   | $0.019     | 2.6M      |
| Sonnet 4.6      | 57.4%  | $1.15   | $0.020     | 427K      |
| GPT-5.2         | 60.5%  | $1.25   | $0.021     | 462K      |
| GPT-5.3 Codex   | 55.7%  | $1.69   | $0.030     | 626K      |
| GPT-5.4         | 47.5%  | $2.01   | $0.042     | 744K      |
| Gemini 3.1 Pro  | 52.1%  | $2.04   | $0.039     | 755K      |
| Opus 4.6        | 59.8%  | $3.11   | $0.052     | 1.2M      |
| Opus 4.7        | 55.9%  | $4.18   | $0.075     | 3.1M      |
| GPT-5.5         | 47.9%  | $5.63   | $0.118     | 4.2M      |

## Key Findings (Verbatim Themes)

- **GPT-5.2 and Claude Opus 4.6 lead** at ~60% F1, but GPT-5.2 is 2.5× cheaper.
- **Newer ≠ better.** GPT-5.4 is too conservative (high precision, low recall);
  GPT-5.5 is too noisy (~half of comments are false positives).
- **OSS punches above its weight.** Kimi K2.5 (51.9% F1 @ $0.41) and GLM-5.1
  (55.8% @ $1.06) compete with frontier models at a fraction of the cost.
- **Cost explains only ~21% of quality variance.** Architecture and training
  matter far more than token budget.
- **Compound strategies** (multi-pass, ensemble voting, deep-dive on high-risk
  files) with cheap models look competitive — eight MiniMax M2.7 runs cost
  less than one GPT-5.2 run.

## Implications for the Dorf Review Block

These are pointers, not commitments — they should be debated against
[principles.md](../project/principles.md) before they become design.

- **Two-pass review is the durable primitive.** Recall first (candidates),
  precision second (validator). Worth modelling explicitly in dorf's
  review pipeline rather than relying on a single agent prompt.
- **Reasoning effort is a first-class input.** Default to `high` on the review
  pass; expose it as a knob so we can A/B against `medium` once we have our
  own golden set.
- **Skill-driven rubric + structured output.** Keep the review rubric and
  output format in a versioned skill file the agent loads, not buried in
  glue code. Mirrors Factory's `code-review` skill.
- **Severity tags + machine-parseable findings.** `[P0|P1|P2]` tagged JSON
  lets the validator pass and downstream UI consume reviews deterministically.
- **Model selection is per-task, not global.** GPT-5.2 for "best overall",
  Kimi K2.5 / GLM-5.1 / Gemini 3 Flash for cheap multi-pass strategies. We
  should let `.dorf.toml` override model + reasoning effort per repo.
- **Build our own golden set early.** Factory's results show the answer is
  workload-specific (e.g., Kimi K2.5 has the best precision in the table at
  71.5%, but worst recall). For dorf repos we should curate a small
  golden set of past bugs to score model choice against our actual code.
- **Watch token cost, not just price/PR.** GPT-5.5 burns 4.2M tokens/PR for
  worse F1 than GPT-5.2 at 462K — token budget alone is not a quality signal.
- **Compound passes beat one frontier pass for the cost.** Worth experimenting
  with N-of-K ensemble voting on a cheap model once the single-pass pipeline
  is stable.
