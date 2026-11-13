# Dorf North Star

**Hand a coding task to an agent that works in its own Linux machine.**

Dorf protects human attention: every choice minimizes friction and maximizes delight.

Dorf is a self-hosted coding agent. Each Thread gets its own Sandbox: an isolated Linux machine
with internet access that keeps its disk, memory and running processes while paused, costs almost nothing while
paused, and resumes on the next tool call. The agent loop and its history run outside the
Sandbox; only the tools run inside it. Upgrades are scripted and harmless to running work.

Dorf releases up to 0.31.3 were a different product, a control plane for third-party agent
harnesses. That code is frozen at the `v0.31.3` tag and is not maintained.

## Shape

```text
+-------------------------------------------+     +---------------------------+
|  agent  (TypeScript)                      |     |  deploy/substrate.sh      |
|  Threads | Pi Durable Harness + SQLite    |     |  install, upgrade and     |
|  pauses a Sandbox after its run ends      |     |  rollback of Substrate,   |
|  drains Sandboxes off workers going away  |     |  from released images     |
+---------------------+---------------------+     +---------------------------+
                      | HTTP: create | pause | delete    WebSocket: tools through pi-env
                      v
+-------------------------------------------+
|  gate (in the cluster)                    |
|  the agent's token decides; Substrate API |
+---------------------+---------------------+
                      | a request resumes a paused Sandbox
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
- A Thread runs on the model and thinking level picked when it starts; the thinking level can change
  later. It can start persistent subagents, Pi's pattern, that share its Sandbox and may run on
  another offered model.
- A Thread reads the AGENTS.md files and `.agents/skills` in its Sandbox, from the root down to its
  working directory, before each request, as Pi's coding agent does.
- Each bash call is a job the Sandbox owns; the agent's stream is only a view of it, so a lost
  stream, a pause or an agent restart reattaches instead of failing the call.
- [Agent Substrate](https://github.com/agent-substrate/substrate) owns isolation, memory
  snapshots, routing and automatic resume. Dorf adds only what Substrate leaves to its user: when
  to pause, which Sandbox pauses when a worker is full, and pausing Sandboxes before their worker
  goes away.
- Neither Sandboxes nor the agent hold credentials. The broker adds the model credential and the
  GitHub token to the agent's few allowed requests, and the egress gateway adds the GitHub token
  to a Sandbox's requests to GitHub.

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Thread** | One line of work: a transcript and at most one Sandbox. In Pi-facing code it is a root `Conversation`. |
| **Sandbox** | The isolated machine where a Thread's tools run. Keeps disk, memory and processes while paused. |
| **create, resume, pause, delete** | The Sandbox operations Dorf performs. The agent resumes a Sandbox before it connects. |
| **make room** | When no worker has room for a Sandbox to resume, pausing the Sandbox whose Thread has been idle the longest. With every Sandbox busy, the resume waits for a run to end. |
| **archive** | As in Amp: a Thread leaves the list, its work stops and its Sandbox pauses at once; a message brings it back. Delete removes the Thread's Sandbox; Pi keeps its transcript. |
| **Signal** | Something that happened that a Thread may act on: a CI result, a held deploy, a labelled issue, a time of day. A Thread follows the signals it cares about, set up from plain words in the Thread. A signal wakes each Thread that follows it, or starts a new one. |
| **drain** | Pausing every running Sandbox on a worker that is going away, even mid-call, so an upgrade loses no work. Substrate leaves it to the Sandbox's driver, the agent. |

Substrate's own terms (Actor, Worker, Atespace, ActorTemplate) appear only in the gate and in
deployment files.

## Boundaries

- Only the gate knows Substrate. It runs in the cluster and acts with the agent's own token, which
  Substrate scopes to Dorf's atespace. The agent's Sandbox module knows only the gate; Thread code
  knows a Sandbox ID and nothing else.
- The agent sees a machine: tools reach a Sandbox only through pi-env. Where the agent runs is a
  deployment choice; it needs only HTTP and WebSocket to the gate.
- The Sandbox image holds no credentials and knows nothing about Dorf.
- Signals reach Threads only through the agent's Thread API, whatever carries them there.
- Code moves into a shared package only when a second app imports it.

## Deleted, and when to add it back

| Deleted | Add back when |
| --- | --- |
| A separate Sandbox service | A second app owns Sandboxes |
| Per-call leases | Never. The agent pauses a Sandbox a few minutes after its run ends |
| Draining from outside the agent | Sandboxes can be awake while no agent runs: the agent is down during an upgrade, its host can stop it between requests, or it has more than one replica |
| Our own Postgres and migrations | The agent needs more than one replica |
| App authentication, tunnels, enrolled CLIs | Access from outside the cluster; then put an existing access proxy in front |
| Code mode | A measured need |
| The microVM class and restricted egress | Running code from people we do not trust, such as pull requests from forks |
| Silently replacing a deleted Sandbox | Never. Tools fail with "Sandbox not found" |
| A dashboard | Daily use asks for more than a minimal chat page |
| A `.agents/resume` hook that a Thread's Sandbox runs after starting from its repository's snapshot | A repository needs a process running in every Thread. Until then setup must leave none, since the snapshot's memory restores it into every Thread |
| Provider adapters, sessions, profiles, the Provider Gateway, durable message custody | Never as before. These served the 0.31 control plane |

## Non-goals

Dorf is not a control plane for other agents, a provider-neutral sandbox layer, a workflow engine,
a hosted service or an SDK. Interfaces are extracted from working code when a second caller needs
them, not designed ahead of it.

## Proof

- **One Thread, one Sandbox.** A tool call writes a file and starts a background process; the
  Sandbox pauses after the run; the next message resumes it with the file and process intact; an
  agent killed mid-call finishes after restart; archiving pauses the Sandbox at once and a message
  brings the Thread back; deleting the Thread deletes its Sandbox.
- **Harmless upgrades.** A Substrate upgrade and rollback run with zero hand steps while a Thread
  is live, and no Sandbox loses work.
- Tests use Pi's faux model; real model keys never appear in tests or CI.

Slices and their status are in [slices.md](slices.md).
