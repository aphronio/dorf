---
name: show-me
description: "Show a change, a design or a finding as the smallest picture that proves it (a call or file tree, diff, Mermaid diagram, table, or before and after screenshots) with little text. Use for /show-me, 'show me', and every reply that reports finished work."
---

Help the user understand the current topic of conversation visually. Skip the preamble and keep prose brief. Pick the smallest view that makes the key point clear.

- Show logic or an algorithm as pseudocode:

```text
on(save)
  if content is unchanged
    return cached result
  write new content
  return fresh result
```

- Show runtime control flow as a call tree:

```text
submitForm
  createSession
    persistPrompt
    launchAgent
  navigateToSession
```

- Show UI structure as a component tree, including state and module boundaries that matter:

```tsx
<SessionPage> (apps/example/src/routes/session.tsx)
  useSessionEvents()
  <SessionToolbar>
    <RunSkillButton> (packages/ui)
```

- Show file responsibility or a broad refactor as a shallow file tree:

```text
src/
├── commands/       # parses user actions
├── sessions/       # owns session state
└── transport/      # sends API requests
```

- Show component interaction, control flow, or data flow with Mermaid:

```mermaid
sequenceDiagram
    participant User
    participant UI
    participant Daemon
    User->>UI: choose command
    UI->>Daemon: send expanded prompt
    Daemon-->>UI: stream result
```

- Compare options, measurements or states in a table.

- Show a visible change to a page as two screenshots of the same view, before and after, taken with the `browser-harness` skill and saved outside the repository. Place each with its absolute path, as `![after](/tmp/after.png)`. Before the first, set the tab to twice the pixels, so the owner's high-density screen shows it sharp, and at most 1000 CSS pixels a side, so `read` keeps it under 2000 pixels and does not shrink it:

  ```python
  cdp("Emulation.setDeviceMetricsOverride", width=1000, height=640, deviceScaleFactor=2, mobile=False)
  ```

- Use `diff` when the point is what changes and the surrounding shape already exists. Match the diff shape to the topic.

For a component change:

```diff
 <SessionPage>
   useSessionEvents()
   <SessionToolbar>
+    <RunSkillButton />
   <SessionTimeline>
+    <SkillResultCard />
```

For a file-layout change:

```diff
 src/
 ├── commands/
+│   └── show-me.ts       # expands the slash command
 ├── sessions/
-└── transport.ts
+└── transport/
+    ├── client.ts
+    └── stream.ts
```

For a call-tree or call-stack change:

```diff
 submitForm
   createSession
     persistPrompt
+    expandSkillMention
     launchAgent
-  navigateToSession
+  navigateToSession
+    subscribeToEvents
```

For a state or control-flow change:

```diff
 on(save)
-  write content
+  if content is unchanged
+    return cached result
+  write new content
+  invalidate cache
```

- Show the whole block when most of it is new, when omitted context would hide ownership or order, or when the user needs a copyable target shape:

```ts
function expandSkill(command: string): string {
  const skillName = command.slice(1)
  return `use the ${skillName} skill`
}
```

### guidance

Place each visual next to the short text it supports. Keep only the calls, files, props, states, and boundaries needed to answer the user's current question or the options to resolve the current discussion point.

You may use one of these, you may use several, it is unlikely you will use all of them. Use your judgement and don't overwhelm the user. Size the proof to the change: a one-line fix needs one line, not a diagram.


<!-- From humanlayer/skills@701b6bcb, plugins/rpi/skills/rpi/references/show-me.md, without the HTML file a Sandbox cannot open, and with tables, screenshots and sizing added. Copyright (c) 2026 HumanLayer, MIT License; see ../LICENSE-humanlayer. -->
