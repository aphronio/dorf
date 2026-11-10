import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { type AgentChange, type ConversationId, defineDoc, type Harness, type HarnessOptions } from "@earendil-works/pi-durable";
import { pauseWhenIdle } from "./idle.ts";
import type { Sandboxes } from "./sandbox.ts";

// `fork: "initial"`: a fork gets no Sandbox until one is assigned.
const ThreadSandbox = defineDoc<{ name?: string }>({
	kind: "dorf.sandbox",
	version: 1,
	scope: "conversation",
	history: "latest",
	fork: "initial",
	initial: () => ({}),
	checkpointWhen: () => false,
});

export type ThreadSummary = { id: ConversationId; title: string; createdAt: string };

const ThreadList = defineDoc<{ items: ThreadSummary[] }>({
	kind: "dorf.threads",
	version: 1,
	scope: "session",
	initial: () => ({ items: [] }),
	checkpointWhen: () => false,
});

export function sandboxEnv(sandboxes: Sandboxes, workspace: string): HarnessOptions["env"] {
	return async ({ conversationId, cwd, read }, envContext) => {
		const sandbox = await read.snapshot(ThreadSandbox, conversationId, envContext);
		return sandbox?.name ? sandboxes.env(sandbox.name, cwd ?? workspace) : undefined;
	};
}

export class Threads {
	readonly #harness: Harness;
	readonly #sandboxes: Sandboxes;
	readonly #agent: AgentChange;
	readonly #idleMs: number;

	constructor(harness: Harness, sandboxes: Sandboxes, agent: AgentChange, idleMs: number) {
		this.#harness = harness;
		this.#sandboxes = sandboxes;
		this.#agent = agent;
		this.#idleMs = idleMs;
	}

	async open(): Promise<void> {
		for (const thread of await this.list()) await this.#watchIdle(thread.id);
	}

	async list(): Promise<ThreadSummary[]> {
		return (await this.#harness.snapshot(ThreadList, context))?.items ?? [];
	}

	async sandbox(id: ConversationId): Promise<string | undefined> {
		return (await this.#harness.snapshot(ThreadSandbox, id, context))?.name;
	}

	// The Thread commits before its Sandbox exists, so no Sandbox is ever left without a Thread.
	async create(prompt: string): Promise<{ id: ConversationId; sandbox: string }> {
		const sandbox = this.#sandboxes.newName();
		const conversation = await this.#harness.createConversation(
			{
				ownership: { kind: "ownerless" },
				agent: this.#agent,
				init: async (tx, id) => {
					(await tx.doc(ThreadSandbox, id)).name = sandbox;
					(await tx.doc(ThreadList)).items.push({ id, title: prompt.slice(0, 80), createdAt: new Date().toISOString() });
				},
			},
			context,
		);
		try {
			await this.#sandboxes.create(sandbox);
		} catch (e) {
			await this.delete(conversation.id);
			throw e;
		}
		await this.#watchIdle(conversation.id);
		await conversation.submit({ type: "input", content: prompt }, context);
		return { id: conversation.id, sandbox };
	}

	async send(id: ConversationId, text: string): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.submit({ type: "input", content: text }, context);
		return true;
	}

	async read(id: ConversationId) {
		const conversation = await this.#known(id);
		if (!conversation) return undefined;
		const watch = await conversation.watch(context);
		const view = watch.value;
		await watch.stop();
		return view;
	}

	// The Sandbox goes before the commit: a crash in between leaves a listed Thread whose
	// retry finishes the job, never an orphan Sandbox. Pi has no way to remove a transcript.
	async delete(id: ConversationId): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.abort(context);
		const sandbox = await this.sandbox(id);
		if (sandbox) await this.#sandboxes.delete(sandbox);
		await this.#harness.commit(async (tx) => {
			const list = await tx.doc(ThreadList);
			list.items = list.items.filter((t) => t.id !== id);
			await tx.retireDoc(ThreadSandbox, id);
		}, context);
		return true;
	}

	#watchIdle(id: ConversationId): Promise<void> {
		return pauseWhenIdle(this.#harness, id, this.#idleMs, async (stillIdle) => {
			const name = await this.sandbox(id);
			if (!name || (await this.#sandboxes.state(name)) !== "running" || !stillIdle()) return;
			await this.#sandboxes.pause(name, "idle");
		});
	}

	async #known(id: ConversationId) {
		return (await this.list()).some((t) => t.id === id) ? this.#harness.conversation(id, context) : undefined;
	}
}
