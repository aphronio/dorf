import { defineExtension, defineTask, type ConversationId, type Tx, type UserInput } from "@earendil-works/pi-durable";
import { checkout, CheckoutFailed, setup } from "./repo-checkout.ts";
import { type CatchUp, RepoSnapshots, type repoSnapshots, startFrom } from "./repo-snapshots.ts";
import { RETRY_ROOM_MS, type Sandboxes } from "./sandbox.ts";
import { ThreadList, type ThreadSummary } from "./thread-list.ts";

export type StartInput = { sandbox: string; repo?: string; workspace: string; cwd: string; content: UserInput };
// `plain` is true once the Sandbox gave up on the repository's snapshot.
type StartState = { phase: "start"; tries?: number; plain?: boolean };

// Errors other than a failed clone, such as the gate restarting, are tried again this many times.
const TRIES = 5;

async function mark(tx: Tx, id: ConversationId, change: (thread: ThreadSummary) => void): Promise<void> {
	const thread = (await tx.doc(ThreadList)).items.find((t) => t.id === id);
	if (thread) change(thread);
}

// The Threads whose Sandboxes run now; set once they exist.
type Holders = () => Promise<ConversationId[]>;
type Hooks = { holders: Holders };

// A new Thread's Sandbox, from its repository's snapshot when there is one, the clone brought to the default branch's
// tip with its setup run, and its first message, after the Thread is committed. While no worker has room the task
// sleeps and asks again, so it waits as long as it takes, across restarts; each ask first pauses the Thread idle the
// longest. A repeat after a restart creates, checks out and sets up again, since the message is not in until the last
// commit. A setup that fails or runs too long is logged and the Thread starts anyway: the agent finds out when it runs
// its checks.
export function threadStart(sandboxes: Sandboxes, snapshots: ReturnType<typeof repoSnapshots>) {
	const hooks: Hooks = { holders: async () => [] };

	const task = defineTask<StartInput, StartState, null>({
		name: "dorf.thread-start",
		version: 1,
		initial: () => ({ phase: "start" }),
		phases: {
			start: async (start, runtime, context) => {
				const { sandbox, repo, workspace, cwd, content } = start.input;
				const id = runtime.conversationId;
				const later = (checkpoint: StartState) => ({ status: "sleeping", until: Date.now() + RETRY_ROOM_MS, checkpoint }) as const;
				const checkpoint = start.state.checkpoint;
				let failure: string | undefined;
				let caughtUp: CatchUp | undefined;
				let plain = checkpoint.plain === true;
				try {
					const snapshot = repo && !plain ? (await runtime.snapshot(RepoSnapshots, context))?.repos[repo]?.snapshot : undefined;
					const started = await startFrom(sandboxes, sandbox, snapshot, true);
					if (snapshot && !started.fromSnapshot) plain = true;
					if (!started.woke) {
						const busy = (await hooks.holders()).filter((h) => h !== id);
						return await runtime.commit(async (tx) => {
							await mark(tx, id, (t) => {
								if (!t.waiting) t.waiting = { since: new Date().toISOString(), busy };
								else if (t.waiting.busy.join() !== busy.join()) t.waiting.busy = busy;
							});
							return later({ phase: "start", plain });
						}, context);
					}
					await runtime.commit(async (tx) => void (await mark(tx, id, (t) => void delete t.waiting)), context);
					if (repo) {
						const began = Date.now();
						const tip = await checkout(sandboxes.env(sandbox, workspace), repo, cwd, context);
						const failed = await setup(sandboxes.env(sandbox, cwd), repo, context);
						if (failed) console.error(failed);
						caughtUp = { fromSnapshot: started.fromSnapshot, tip, ms: Date.now() - began };
					}
				} catch (e) {
					const tries = (checkpoint.tries ?? 0) + 1;
					console.error(`start Thread ${id}: ${(e as Error).message}`);
					if (!(e instanceof CheckoutFailed) && tries < TRIES) return await runtime.commit(() => later({ phase: "start", tries, plain }), context);
					failure = (e as Error).message;
				}
				const thread = (await runtime.conversation(id, context))!;
				await runtime.commit(async (tx) => {
					if (!failure) await thread.submit(tx, { type: "input", content, requestId: `start:${start.id}` });
					if (repo && caughtUp) await snapshots.refresh(tx, { repo, workspace }, caughtUp);
					await mark(tx, id, (t) => {
						delete t.starting;
						delete t.waiting;
						if (failure) Object.assign(t, { failure, attention: "failed", ended: "failed", endedAt: new Date().toISOString() });
					});
					return { status: "terminal", outcome: { status: "completed", result: null } };
				}, context);
			},
		},
		// Only deleting the Thread aborts its start.
		abort: (_start, runtime, context) => runtime.commit(() => ({ status: "terminal", outcome: { status: "aborted" } }), context),
	});
	return Object.assign(hooks, { task, extension: defineExtension({ name: "dorf.thread-start", tasks: [task] }) });
}
