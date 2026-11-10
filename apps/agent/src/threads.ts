import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { type AgentChange, type ConversationId, defineDoc, type Harness, type HarnessOptions, ProviderDoc } from "@earendil-works/pi-durable";
import { REPO } from "./github.ts";
import { pauseWhenIdle } from "./idle.ts";
import type { Sandboxes } from "./sandbox.ts";

const ThreadSandbox = defineDoc<{ name?: string }>({
	kind: "dorf.sandbox",
	version: 1,
	scope: "conversation",
	history: "latest",
	fork: "initial",
	initial: () => ({}),
	checkpointWhen: () => false,
});

export type ThreadSummary = { id: ConversationId; title: string; createdAt: string; repo?: string };

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

// A provider that keeps a process or copy per Thread, keyed by Pi's provider session id.
export type ProviderSessions = { release(sessionId: string): Promise<void>; forget(sessionId: string): Promise<void> };

export class Threads {
	readonly #harness: Harness;
	readonly #sandboxes: Sandboxes;
	readonly #agent: AgentChange;
	readonly #idleMs: number;
	readonly #providerSessions: ProviderSessions;
	readonly #idleSince = new Map<ConversationId, number>();

	constructor(harness: Harness, sandboxes: Sandboxes, agent: AgentChange, idleMs: number, providerSessions: ProviderSessions) {
		this.#harness = harness;
		this.#sandboxes = sandboxes;
		this.#agent = agent;
		this.#idleMs = idleMs;
		this.#providerSessions = providerSessions;
		sandboxes.makeRoom = () => this.#makeRoom();
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
	async create(prompt: string, repo?: string): Promise<{ id: ConversationId; sandbox: string }> {
		if (repo !== undefined && !REPO.test(repo)) throw new RangeError(`not a GitHub repository: ${repo}`);
		const sandbox = this.#sandboxes.newName();
		const workspace = this.#agent.cwd ?? "/";
		const cwd = repo ? `${workspace}/${repo.split("/")[1]}` : workspace;
		const agent = repo ? { ...this.#agent, cwd, instructions: `${this.#agent.instructions}\n\nThe repository ${repo} is cloned in ${cwd}.` } : this.#agent;
		const conversation = await this.#harness.createConversation(
			{
				ownership: { kind: "ownerless" },
				agent,
				init: async (tx, id) => {
					(await tx.doc(ThreadSandbox, id)).name = sandbox;
					(await tx.doc(ThreadList)).items.push({ id, title: prompt.slice(0, 80), createdAt: new Date().toISOString(), ...(repo ? { repo } : {}) });
				},
			},
			context,
		);
		try {
			await this.#sandboxes.create(sandbox);
			if (repo) await this.#clone(sandbox, repo, workspace, cwd);
		} catch (e) {
			await this.delete(conversation.id);
			throw e;
		}
		await this.#watchIdle(conversation.id);
		await conversation.submit({ type: "input", content: prompt }, context);
		return { id: conversation.id, sandbox };
	}

	async #clone(sandbox: string, repo: string, workspace: string, into: string): Promise<void> {
		let output = "";
		const cloned = await this.#sandboxes
			.env(sandbox, workspace)
			.exec(["git", "clone", "--quiet", `https://github.com/${repo}.git`, into], { onOutput: (text) => void (output += text) }, context);
		if (!cloned.ok) throw cloned.error;
		if (cloned.value.exitCode !== 0) throw new Error(`git clone ${repo}: ${output.trim().slice(-500)}`);
	}

	async send(id: ConversationId, text: string): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.submit({ type: "input", content: text }, context);
		return true;
	}

	async compact(id: ConversationId): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.compact(undefined, context);
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

	async abort(id: ConversationId): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.abort(context);
		return true;
	}

	// Pi's JSON state mode: first every entry, then per change the new entries and the live state.
	async follow(id: ConversationId, send: (event: { entries?: readonly unknown[]; live: unknown }) => void): Promise<(() => Promise<unknown>) | undefined> {
		const conversation = await this.#known(id);
		if (!conversation) return undefined;
		const watch = await conversation.watch(context);
		let newest = watch.value.entries.at(-1)?.id ?? 0;
		send({ entries: watch.value.entries, live: watch.value.docs["pi.live"] });
		watch.start(async (call) => {
			if (call.kind !== "value") return;
			const entries = call.value.entries.filter((entry) => entry.id > newest);
			newest = call.value.entries.at(-1)?.id ?? newest;
			send({ ...(entries.length ? { entries } : {}), live: call.value.docs["pi.live"] });
		});
		return () => watch.stop();
	}

	// The Sandbox goes before the commit: a crash in between leaves a listed Thread whose
	// retry finishes the job, never an orphan Sandbox. Pi has no way to remove a transcript.
	async delete(id: ConversationId): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.abort(context);
		const session = await this.#providerSession(id);
		if (session) await this.#providerSessions.forget(session);
		const sandbox = await this.sandbox(id);
		if (sandbox) await this.#sandboxes.delete(sandbox);
		this.#idleSince.delete(id);
		await this.#harness.commit(async (tx) => {
			const list = await tx.doc(ThreadList);
			list.items = list.items.filter((t) => t.id !== id);
			await tx.retireDoc(ThreadSandbox, id);
		}, context);
		return true;
	}

	#watchIdle(id: ConversationId): Promise<void> {
		return pauseWhenIdle(
			this.#harness,
			id,
			this.#idleMs,
			(stillIdle) => this.#sleep(id, stillIdle),
			(running) => (running ? this.#idleSince.delete(id) : this.#idleSince.set(id, Date.now())),
		);
	}

	// Returns whether the Sandbox was running and is now paused.
	async #sleep(id: ConversationId, stillIdle: () => boolean): Promise<boolean> {
		const session = await this.#providerSession(id);
		if (session && stillIdle()) await this.#providerSessions.release(session);
		const name = await this.sandbox(id);
		if (!name || (await this.#sandboxes.state(name)) !== "running" || !stillIdle()) return false;
		await this.#sandboxes.pause(name, "idle");
		return true;
	}

	// The worker is full: the Thread idle the longest gives up its Sandbox before its idle pause.
	async #makeRoom(): Promise<boolean> {
		for (const [id] of [...this.#idleSince].sort((a, b) => a[1] - b[1])) {
			const paused = await this.#sleep(id, () => this.#idleSince.has(id)).catch((e) => {
				console.error(`make room: Thread ${id}: ${e.message}`);
				return false;
			});
			this.#idleSince.delete(id);
			if (paused) {
				console.log(`make room: paused the Sandbox of Thread ${id}`);
				return true;
			}
		}
		return false;
	}

	async #providerSession(id: ConversationId): Promise<string | undefined> {
		return (await this.#harness.snapshot(ProviderDoc, id, context))?.sessionId;
	}

	async #known(id: ConversationId) {
		return (await this.list()).some((t) => t.id === id) ? this.#harness.conversation(id, context) : undefined;
	}
}
