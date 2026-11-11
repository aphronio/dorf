# Native path first

Stand on the shoulders of giants. Before writing adapters, wrappers, protocols, registries, queues or
coordination logic, find the built-in path in the platform, library, runtime or harness already in use.

- Read the current docs. Do not guess from memory or old articles; check the date. Cite the source.
- Use the library's own vocabulary and patterns (its classes, states, terms) instead of inventing
  names.
- Ask "am I reinventing X?" (a queue, a scheduler, a state machine the framework already has).
- Imitate, then innovate: copy a proven design from a strong product or project, then change only
  what your case needs.
- Use the platform's mechanisms instead of fighting them. Hashimoto rewrote Ghostty's GTK app because
  it avoided GTK's own object model and paid in lifetime bugs.
- Test the smallest promising native path before building custom machinery. Choose by complete user
  outcome and total maintenance cost, not by familiarity.
- Fix the environment the proper way (the repo's own runner, dependencies and patterns), not with a
  one-off hack around it.
- Test a tool on your own real inputs before adopting or paying for it.
- Technology is a means. If a library hurts more than it helps, rip it out.

Test: What does the tool we already use provide for this, and did I try it first?
