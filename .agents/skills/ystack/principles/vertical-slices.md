# Vertical slices

Sources: Mitchell Hashimoto, [My Approach to Building Large Technical Projects](https://mitchellh.com/writing/building-large-technical-projects);
Paul Graham, [Do Things That Don't Scale](https://paulgraham.com/ds.html).

Build one thin, complete loop from the user's action to the observed result before generalizing any
part of it. The [slice playbook](../playbooks/slice.md) owns the steps.

- Good enough first: hardcode or keep in memory until real use says otherwise.
- Pull one lever at a time. Changing many things at once hides what helped.
- Fast loop first (one page, one case, one fixture), then the full run.

Test: What can the user do after this slice that they could not do before, and how do I know?
