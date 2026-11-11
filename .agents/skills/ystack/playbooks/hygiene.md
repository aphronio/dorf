# Hygiene playbook

Reduce the facts an agent must remember and the places a maintainer must keep in sync. Leave the
repository with one discoverable command authority, small progressive guidance, and deterministic
checks for facts a machine can decide.

1. Audit before adding tools. Inspect agent instructions, doc entry points, task runners, hooks, CI,
   manifests, generators and generated files. Build a temporary inventory:

   | Instruction or invariant | Current owner | Machine-decidable? | Existing enforcement | Drift cost |
   | --- | --- | --- | --- | --- |

   Search for existing commands and callers before adding a script.
2. Put each concern in one place:
   - Exact commands, paths, versions, generated-file sets and pass/fail logic go in the task runner,
     config or a check script.
   - Product direction, authority, safety, permissions and judgment go in the owning prose doc.
   - Agent and contributor guides route; they link to owners instead of copying inventories.
   - Pair every generator with a non-mutating `check`; wire the check, not the generator, into the gate.
   - Expose one fast gate and one final gate per scope; hooks and CI reuse them.
3. Choose enforcement from evidence:
   - Blocking check: deterministic, low-noise, actionable, fast, stable.
   - Ratchet: valuable metric with existing debt; reject only new debt.
   - Report: needs judgment; keep it out of the gate and give it an owner.
   - Reject: redundant, noisy, slow, easily gamed, or costlier than the failure it prevents.
   Prefer an existing compiler, formatter, linter, schema tool or generator over a custom checker. Do
   not enforce prose taste or file-length limits with regexes.
4. Never let a validation command silently fix source or leave tracked files changed.
5. Run the new checks, then the fast and final gates. Replace stale copies in guides with links.

Reply: facts that gained enforcement, copies removed, candidates kept as reports or rejected (with
evidence), judgment that still lives in prose and where.
