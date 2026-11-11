# Model judges, code enforces

Sources: Anthropic, [Building effective agents](https://www.anthropic.com/engineering/building-effective-agents);
Rich Sutton, [The Bitter Lesson](http://www.incompleteideas.net/IncIdeas/BitterLesson.html).

Let the model do interpretation, planning and judgment. Let code do what must be exact: permissions,
effects, money, timers, validation of external data.

- Keep the harness minimal. Give the model context, tools and durable facts; do not script its
  reasoning or add mandatory steps "to help it".
- Prompts stay general. A rule that fits only one document, customer or test case is overfitting.
- No regex rescue, keyword lists or post-processing to patch model output while tuning. That is
  feature engineering; it is brittle and hides failures.
- Pass stage output to the next stage as-is, like a Unix pipe. Use JSON only where code consumes it.
- Hide internals the model does not need.
- Write prompts, skills and AGENTS.md for the target model's current vendor guide
  ([Anthropic](https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/claude-prompting-best-practices),
  [skills](https://platform.claude.com/docs/en/agents-and-tools/agent-skills/best-practices),
  [OpenAI](https://developers.openai.com/api/docs/guides/prompt-guidance)): calm wording with
  absolutes kept for real invariants, the reason behind each rule, the outcome and stop rule rather
  than a step list, and old scaffolding deleted when models improve.
- Add a corrective instruction only for a repeated, observed failure, after trying better context,
  tools, model or environment. Remove it when the cause goes away.

Test: Am I giving the model a missing fact or capability, or making its decision for it?
