# Dorf North Star — Durable Coding Jobs

Dorf accepts a complete coding goal, carries it durably through an isolated Sandbox, uses programs
for knowable work and agents for judgment, and returns an exact-Revision proposal with evidence.

This is the product and experience direction. It is not an API inventory, schema, package plan, or
issue backlog. Concrete implementation follows the greenfield architecture and live GitHub epic.

## Ten-second model

```text
A Job changes code in a Sandbox.
Actions and Checks do deterministic work.
AgentRuns do judgment.
Evidence proves claims about a Revision.
```

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Job** | The durable user goal and lifecycle, from admission to explicit terminal outcome |
| **Sandbox** | The isolated mutable machine and checkout boundary for one Job |
| **Revision** | The exact immutable commit being implemented, checked, reviewed, or proposed |
| **AgentRun** | One bounded invocation of an agent against a Job and Revision |
| **Session** | Optional Job-scoped conversational continuity across implementation AgentRuns |
| **Role** | The bounded responsibility of an AgentRun, such as implement, general review, QA, or security |
| **Action** | Code-owned work that changes external state |
| **Check** | Code-owned observation or assertion |
| **Evidence** | Immutable, provenance-labelled proof tied to a Revision or lifecycle fact |

`Role` is a field, not an executing object. There is no `RoleRun`. An agent executes in a Role.
There is no first-class Worker until identity or memory must survive across Jobs.

## High-level flow

```mermaid
flowchart TD
    Intent["Complete coding goal"] --> Admit["Deterministic admission"]
    Admit --> Job["Create durable Job"]
    Job --> Sandbox["Create Sandbox and clone"]
    Sandbox --> Setup["Run repository setup contract"]
    Setup --> Implement["Implementation AgentRun changes and commits"]
    Implement --> Revision["Observe clean final descendant Revision"]
    Revision --> Checks["Deterministic Checks"]
    Checks --> Facts["Compute ChangeFacts"]
    Facts --> Policy{"ReviewPolicy"}
    Policy -->|"known roles"| Reviews["Selected review AgentRuns"]
    Policy -->|"unknown"| GeneralReview["General review AgentRun"]
    Policy -->|"no review"| Proposal
    Reviews --> Feedback["Reviewer text becomes a Message"]
    GeneralReview --> Feedback
    Feedback --> Respond["Original implementation Session decides what to do"]
    Respond -->|"committed change"| Revision
    Respond -->|"clean unchanged checkout"| Proposal["Push and propose exact Revision"]
    Proposal --> Observe["Observe exact pull request"]
    Observe -->|"trusted comment"| Feedback
    Observe -->|"merged or closed"| Outcome{"Accept, reject, or abandon"}
    Outcome --> Cleanup["Reconcile cleanup"]
    Cleanup --> Receipt["Evidence-backed terminal receipt"]
```

Review is optional and explainable. Deterministic policy selects known specialist Roles and uses one
general reviewer for an unknown change. Each reviewer returns ordinary text. Dorf sends that text as
a Message to the original implementation Session; it does not parse a universal review result. The
implementation agent decides whether to act, explain, or leave the Revision unchanged. No agent
replaces the durable Job as coordinator.

The pull request is the acceptance UI. A comment from the repository owner or a collaborator becomes
an ordinary human Message to the same implementation Session. Merging the exact pull request accepts
the Job; closing it without merging rejects the Job. Explicit abandonment remains a Dorf command.

## The deterministic and agentic boundary

| Programmatic and deterministic | Agentic judgment |
| --- | --- |
| Validate input and authority | Understand the goal and codebase |
| Allocate stable identities, FIFO follow order, and explicit steer priority | Design and implement the change |
| Create, inspect, and destroy Sandboxes | Resolve ambiguity with documented assumptions |
| Clone, set up, observe the final Revision, diff, push, and publish | Decide whether an unfamiliar change needs specialist review |
| — | Create one or many commits when the implementation Session changes code |
| Run declared tests, linters, smoke checks, and probes | Perform QA, security, architecture, or performance review |
| Compute ChangeFacts and apply known review rules | Decide what to do with user, Check, or reviewer Messages |
| Hash, pin, invalidate, and render Evidence | Explain material decisions and remaining uncertainty |
| Retry, reconcile, and report external effects | — |

This boundary is a product promise: agent context is not spent rediscovering facts that code can
establish, and deterministic mechanisms do not pretend to answer questions requiring judgment.

## Desired experience

- **One delegation:** the user supplies a complete goal, not orchestration instructions.
- **Detached by default:** watching a terminal or token stream is optional.
- **Calm recovery:** client, controller, and task-executor loss pause progress but do not erase accepted
  input or create competing Jobs.
- **Situation first:** inspection shows current goal, observed state, claims, Evidence, and required
  attention before logs.
- **Precise interruption:** humans are asked only for consequential boundary decisions or genuine
  ambiguity without a safe default.
- **One proposal:** a coding Job owns one Sandbox, branch, Revision line, and pull-request proposal.
- **Honest terminal:** completion, rejection, abandonment, and cleanup are separate observable facts
  until all have converged.

## Layers and ownership

```text
L0  Existing tools       Git, GitHub, Incus, agent harnesses, provider SDKs
L1  Deterministic edge   Actions, Checks, Sandbox and Agent adapters
L2  Durable core         Job facts, inbox, Absurd execution, recovery
L3  Coding workflow      admission, implementation, review policy, evidence, proposal, terminal
L4  Clients              CLI first; other trusted clients later
```

The durable core owns mechanisms and facts. The coding workflow owns coding semantics and review
policy. Adapters translate existing tools; they do not create a second workflow. Clients invoke and
render the same application boundary; they do not become authorities.

## Non-goals until evidence demands them

- persistent Worker personalities or cross-Job memory;
- multiple simultaneous Jobs sharing one mutable Sandbox;
- a general workflow builder or user-programmable DAG;
- a provider or agent plugin marketplace;
- speculative Sandbox, agent, model, or language matrices;
- mandatory review after every change;
- Dagger or another nested build runtime for repositories whose direct commands suffice;
- fleet dashboards, organizational metaphors, or autonomous agent hierarchies;
- compatibility with the superseded Python, SQLite, Worker, Room, or Assignment implementation;
- preservation or migration of old local data.

## Proof that the North Star is real

A stranger on a clean machine can delegate one real change and later inspect an evidence-backed
pull-request proposal. During the run, the controller and task executor can be killed and restarted,
a message can be accepted while the agent is active, selected review can feed the same implementation
Session through a Message, and cleanup can be retried. No duplicate Job, Sandbox, agent turn, branch,
or pull request appears, and the user
never has to understand Absurd, PostgreSQL rows, Incus names, or harness protocol details.
