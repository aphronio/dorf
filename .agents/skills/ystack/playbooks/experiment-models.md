# Experiment recipe: models and inference

Use with the [experiment playbook](experiment.md) when the subject is a model, quant, inference
engine, sampling setting or prompt.

- Gate: compare full next-token distributions (KL divergence, top-1 agreement) against the exact or
  unquantized path, on prompts of the sizes the real work uses. Matching output text only proves
  determinism, not quality.
- Determinism: temperature 0 and thinking off for speed runs. Check that an arm repeats byte for byte
  before reading a text change as a quality change; some fast paths are not deterministic.
- Measure the phases apart: prompt reading (prefill), generation (decode) at short and long context,
  load time, time to first token. They move independently.
- Real workload: replay agent turns with the context sizes and shared prefixes the owner's agents
  send. Short-chat decode is the least useful number.
- Memory tiers: page cache, pinned RAM and VRAM shared with the desktop all move results. Note what
  else held memory, and re-warm or flush caches per arm.
- Reasoning level: record it with every quality claim. Quantized models can lose much more at lower
  effort.
- Public benchmarks: cite who ran them, separate measured from estimated, and say which precision was
  measured (the full model or the quant you run). A similar index score does not make two models the
  same model.
