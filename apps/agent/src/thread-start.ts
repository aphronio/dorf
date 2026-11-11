import type { Context } from "@earendil-works/chord/context";
import { defineExtension, defineTask, type ConversationId, type Tx, type UserInput } from "@earendil-works/pi-durable";
import { RETRY_ROOM_MS, type Sandboxes } from "./sandbox.ts";
import { ThreadList, type ThreadSummary } from "./thread-list.ts";

export type StartInput = { sandbox: string; repo?: string; workspace: string; cwd: string; content: UserInput };
type StartState = { phase: "start"; tries?: number };

// Errors other than a failed clone, such as the gate restarting, are tried again this many times.
const TRIES = 5;

class CloneFailed extends Error {}

async function mark(tx: Tx, id: ConversationId, change: (thread: ThreadSummary) => void): Promise<void> {
	const thread = (await tx.doc(ThreadList)).items.find((t) => t.id === id);
	if (thread) change(thread);
}

// The Threads whose Sandboxes run now; set once they exist.
type Holders = () => Promise<ConversationId[]>;

// A new Thread's Sandbox, its clone of the repository and its first message, after the Thread is committed. While no
// worker has room the task sleeps and asks again, so it waits as long as it takes, across restarts; each ask first
// pauses the Thread idle the longest. A repeat after a restart creates and clones again, since the message is not in
// until the last commit.
export function threadStart(sandboxes: Sandboxes) {
	const hooks: { holders: Holders } = { holders: async () => [] };
	async function clone(sandbox: string, repo: string, workspace: string, into: string, context: Context): Promise<void> {
		let output = "";
		const script = 'rm -rf "$1" && git clone --quiet "$2" "$1"';
		const cloned = await sandboxes.env(sandbox, workspace).exec(["sh", "-c", script, "_", into, `https://github.com/${repo}.git`], { onOutput: (text) => void (output += text) }, context);
		if (!cloned.ok) throw cloned.error;
		if (cloned.value.exitCode !== 0) throw new CloneFailed(`git clone ${repo}: ${output.trim().slice(-500)}`);
	}

	const task = defineTask<StartInput, StartState, null>({
		name: "dorf.thread-start",
		version: 1,
		initial: () => ({ phase: "start" }),
		phases: {
			start: async (start, runtime, context) => {
				const { sandbox, repo, workspace, cwd, content } = start.input;
				const id = runtime.conversationId;
				const later = (checkpoint: StartState) => ({ status: "sleeping", until: Date.now() + RETRY_ROOM_MS, checkpoint }) as const;
				let failure: string | undefined;
				try {
					await sandboxes.create(sandbox);
					if (!(await sandboxes.wake(sandbox))) {
						const busy = (await hooks.holders()).filter((h) => h !== id);
						return await runtime.commit(async (tx) => {
							await mark(tx, id, (t) => {
								if (!t.waiting) t.waiting = { since: new Date().toISOString(), busy };
								else if (t.waiting.busy.join() !== busy.join()) t.waiting.busy = busy;
							});
							return later({ phase: "start" });
						}, context);
					}
					await runtime.commit(async (tx) => void (await mark(tx, id, (t) => void delete t.waiting)), context);
					if (repo) await clone(sandbox, repo, workspace, cwd, context);
				} catch (e) {
					const tries = (start.state.checkpoint.tries ?? 0) + 1;
					console.error(`start Thread ${id}: ${(e as Error).message}`);
					if (!(e instanceof CloneFailed) && tries < TRIES) return await runtime.commit(() => later({ phase: "start", tries }), context);
					failure = (e as Error).message;
				}
				const thread = (await runtime.conversation(id, context))!;
				await runtime.commit(async (tx) => {
					if (!failure) await thread.submit(tx, { type: "input", content, requestId: `start:${start.id}` });
					await mark(tx, id, (t) => {
						delete t.starting;
						delete t.waiting;
						if (failure) Object.assign(t, { failure, attention: "failed" });
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
