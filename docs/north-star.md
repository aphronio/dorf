# Dorf North Star

**Hand a coding task to an agent that works in its own Linux machine.**

Dorf is a self-hosted coding agent. Each Thread gets its own Sandbox: an isolated Linux machine
that keeps its disk, memory and running processes while paused, costs almost nothing while
paused, and resumes on the next tool call. The agent loop and its history run outside the
Sandbox; only the tools run inside it. Upgrades are scripted and harmless to running work.

Dorf releases up to 0.31.3 were a different product, a control plane for third-party agent
harnesses. That code is frozen at the `v0.31.3` tag and is not maintained.

## Shape

```text
+-------------------------------------------+     +---------------------------+
|  agent  (TypeScript, the only process)    |     |  upgrade  (script)        |
|  Threads | Pi Durable Harness + SQLite    |     |  upgrade and rollback     |
|  pauses a Sandbox after its run ends      |     |  drains Sandboxes off     |
|  calls Substrate: create | pause | delete |     |  terminating workers      |
+---------------------+---------------------+     +---------------------------+
                      | tools through pi-env; a request resumes a paused Sandbox
                      v
+-------------------------------------------+
|  Agent Substrate on Kubernetes (gVisor)   |
|  [Sandbox] [Sandbox] [Sandbox]            |
|  image: tools + pi-env daemon + init      |
+-------------------------------------------+
```

- The agent is built from [Pi Durable](https://github.com/earendil-works/pi) and follows its own
  patterns: the Thread's Sandbox is an app document written in the creating commit, and the
  Harness `env` function looks it up for every tool call.
- [Agent Substrate](https://github.com/agent-substrate/substrate) owns isolation, memory
  snapshots, routing and automatic resume. Dorf adds only what Substrate leaves to its user: when
  to pause, and pausing Sandboxes before their worker goes away.
- Model credentials stay in the agent process. Sandboxes hold no credentials.

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Thread** | One line of work: a transcript and at most one Sandbox. In Pi-facing code it is a root `Conversation`. |
| **Sandbox** | The isolated machine where a Thread's tools run. Keeps disk, memory and processes while paused. |
| **create, pause, delete** | The three Sandbox operations Dorf performs. A paused Sandbox resumes when a request reaches it. |
| **drain** | Pausing every Sandbox on a worker that is about to terminate, so an upgrade loses no work. |

Substrate's own terms (Actor, Worker, Atespace, ActorTemplate) appear only in the agent's
Substrate module and in deployment files.

## Boundaries

- Only the agent's Substrate module knows Substrate. Thread code knows a Sandbox ID and nothing else.
- The Sandbox image holds no credentials and knows nothing about Dorf.
- A later trigger app (GitHub, CI) knows only the agent's Thread API.
- Code moves into a shared package only when a second app imports it.

## Deleted, and when to add it back

| Deleted | Add back when |
| --- | --- |
| A separate Sandbox service | A second app owns Sandboxes, or something other than our upgrade script terminates workers (managed node upgrades, an autoscaler, Spot nodes) |
| Per-call leases | Never. The agent pauses a Sandbox a few minutes after its run ends |
| Pausing the least recently used Sandbox to make room | A Thread actually fails for lack of a free worker |
| An always-on drain loop | Same condition as the Sandbox service |
| Our own Postgres and migrations | The agent needs more than one replica |
| App authentication, tunnels, enrolled CLIs | Access from outside the cluster; then put an existing access proxy in front |
| Code mode | A measured need |
| The microVM class | Running code from people we do not trust, such as pull requests from forks |
| Silently replacing a deleted Sandbox | Never. Tools fail with "Sandbox not found" |
| Archiving a Thread | With the chat page. Then follow Amp: archive hides the Thread and pauses its Sandbox at once; delete removes both |
| A dashboard | Daily use asks for more than a minimal chat page |
| Provider adapters, sessions, profiles, the Provider Gateway, durable message custody | Never as before. These served the 0.31 control plane |

## Non-goals

Dorf is not a control plane for other agents, a provider-neutral sandbox layer, a workflow engine,
a hosted service or an SDK. Interfaces are extracted from working code when a second caller needs
them, not designed ahead of it.

## Proof

- **One Thread, one Sandbox.** A tool call writes a file and starts a background process; the
  Sandbox pauses after the run; the next message resumes it with the file and process intact; an
  agent killed mid-call finishes after restart; deleting the Thread deletes its Sandbox.
- **Harmless upgrades.** A Substrate upgrade and rollback run with zero hand steps while a Thread
  is live, and no Sandbox loses work.
- Tests use Pi's faux model; real model keys never appear in tests or CI.

Slices and their status are in [slices.md](slices.md).
