import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { type AgentChange, type Conversation, type ConversationId, ConversationTranscript, type ConversationView, type EntryId, type ModelRef, defineDoc, type Harness, type HarnessOptions, LiveDoc, ProviderDoc } from "@earendil-works/pi-durable";
import type { ModelThinkingLevel } from "@earendil-works/pi-ai";
import { REPO } from "./github.ts";
import { pauseWhenIdle } from "./idle.ts";
import type { Sandboxes } from "./sandbox.ts";
import { Subagents, subagentsOf } from "./subagent.ts";
import { ThreadSandbox } from "./thread-sandbox.ts";

export type ThreadAgent = { model?: ModelRef; thinkingLevel: ModelThinkingLevel };
export type SubagentView = { name: string; model?: ModelRef; working: boolean };
type FollowEvent = { entries?: ConversationView["entries"]; earlier?: boolean; live?: unknown; agent?: ThreadAgent; subagents?: SubagentView[] };

// Entries per page: the first window, each fill of a gap, each "load earlier".
const PAGE = 50;

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

const agentOf = async (conversation: { agent(c: typeof context): Promise<ThreadAgent> }): Promise<ThreadAgent> => {
	const { model, thinkingLevel } = await conversation.agent(context);
	return { model, thinkingLevel };
};

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
	async create(prompt: string, repo: string | undefined, choice: { model: ModelRef; thinkingLevel: ModelThinkingLevel }): Promise<{ id: ConversationId; sandbox: string }> {
		if (repo !== undefined && !REPO.test(repo)) throw new RangeError(`not a GitHub repository: ${repo}`);
		const sandbox = this.#sandboxes.newName();
		const workspace = this.#agent.cwd ?? "/";
		const cwd = repo ? `${workspace}/${repo.split("/")[1]}` : workspace;
		const agent = { ...this.#agent, ...choice, cwd };
		const conversation = await this.#harness.createConversation(
			{
				ownership: { kind: "ownerless" },
				agent,
				init: async (tx, id) => {
					Object.assign(await tx.doc(ThreadSandbox, id), { name: sandbox, ...(repo ? { repo } : {}) });
					await tx.doc(Subagents, id);
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
		await conversation.submit({ type: "input", content: text, whenBusy: "steer" }, context);
		return true;
	}

	async agent(id: ConversationId): Promise<ThreadAgent | undefined> {
		const conversation = await this.#known(id);
		return conversation && agentOf(conversation);
	}

	// Applies from the Thread's next model request.
	async think(id: ConversationId, thinkingLevel: ModelThinkingLevel): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.configure({ thinkingLevel }, context);
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

	// Pi's JSON state mode over a Thread or, by name, its subagent: first the newest entries and whether earlier ones
	// exist, or after `after` the entries the page missed, then per change the new entries and the live state. The agent
	// goes along when it changes, and for a Thread the list of its subagents when one starts or one's work starts or ends.
	async follow(id: ConversationId, name: string | undefined, after: EntryId | undefined, send: (event: FollowEvent) => void): Promise<(() => Promise<unknown>) | undefined> {
		const conversation = await this.#conversation(id, name);
		return conversation && this.#follow(conversation, after, send, name === undefined);
	}

	// The page of entries before `before`, oldest first, and whether earlier ones remain.
	async earlier(id: ConversationId, name: string | undefined, before: EntryId): Promise<{ entries: readonly unknown[]; earlier: boolean } | undefined> {
		const conversation = await this.#conversation(id, name);
		if (!conversation) return undefined;
		const page = await conversation.entries({ maxEntryId: (before - 1) as EntryId }, PAGE, undefined, context);
		return { entries: [...page.items].reverse(), earlier: page.next !== undefined };
	}

	async #follow(conversation: Conversation, after: EntryId | undefined, send: (event: FollowEvent) => void, withSubagents: boolean): Promise<() => Promise<unknown>> {
		// A reconnect resumes right after the page's newest entry, as Pi's `ConversationTranscript.resume` does.
		const watch = await conversation.watch(context, after === undefined ? undefined : { from: (after + 1) as EntryId });
		// Pi's client transcript over the windowed view: a gap that a stall left is filled before anything is sent, so
		// the page gets every entry after the first window, and pages earlier ones in itself.
		const transcript = new ConversationTranscript((query, limit, ctx) => conversation.entries(query, limit, undefined, ctx), { pageSize: PAGE });
		let sent = after;
		const fresh = async (view: ConversationView) => {
			let { items } = transcript.merge(view);
			for (let gap = items.find((i) => i.kind === "gap"); gap?.kind === "gap"; gap = items.find((i) => i.kind === "gap")) ({ items } = await transcript.fillGap(gap, context));
			// A drop longer than the window resumes at the newest entries; the ones before reach back to the page's.
			const oldest = () => items.find((i) => i.kind === "entry")?.entry.id;
			while (sent !== undefined && items[0]?.kind === "earlier" && (oldest() ?? 0) > sent + 1) ({ items } = await transcript.loadEarlier(context));
			const entries = items.flatMap((i) => (i.kind === "entry" && (sent === undefined || i.entry.id > sent) ? [i.entry] : []));
			sent = transcript.newest ?? sent;
			return { entries, earlier: items[0]?.kind === "earlier" };
		};
		const children = new Map<ConversationId, SubagentView & { stop: () => Promise<unknown> }>();
		const subagents = () => [...children.values()].map(({ name, model, working }) => ({ name, model, working }));
		const watchChildren = async (agents: Record<string, { conversationId: ConversationId }>) => {
			let started = false;
			for (const [name, { conversationId }] of Object.entries(agents)) {
				if (children.has(conversationId)) continue;
				started = true;
				const child = (await this.#harness.conversation(conversationId, context))!;
				const childLive = (await this.#harness.watchDoc(LiveDoc, conversationId, context))!;
				const view = { name, model: (await child.agent(context)).model, working: Boolean(childLive.value.run), stop: () => childLive.stop() };
				children.set(conversationId, view);
				childLive.start(async (call) => {
					if (call.kind !== "value" || Boolean(call.value.run) === view.working) return;
					view.working = Boolean(call.value.run);
					send({ subagents: subagents() });
				});
			}
			return started;
		};
		const registry = withSubagents ? await this.#harness.watchDoc(Subagents, conversation.id, context) : undefined;
		if (registry) await watchChildren(registry.value.agents);
		let agent = watch.value.docs["pi.agent"];
		const { entries, earlier } = await fresh(watch.value);
		send({ entries, ...(after === undefined ? { earlier } : {}), live: watch.value.docs["pi.live"] ?? {}, agent: await agentOf(conversation), ...(withSubagents ? { subagents: subagents() } : {}) });
		watch.start(async (call) => {
			if (call.kind !== "value") return;
			const { entries } = await fresh(call.value);
			const agentChanged = call.value.docs["pi.agent"] !== agent;
			agent = call.value.docs["pi.agent"];
			send({ ...(entries.length ? { entries } : {}), live: call.value.docs["pi.live"] ?? {}, ...(agentChanged ? { agent: await agentOf(conversation) } : {}) });
		});
		registry?.start(async (call) => {
			if (call.kind === "value" && (await watchChildren(call.value.agents))) send({ subagents: subagents() });
		});
		return () => Promise.all([watch.stop(), registry?.stop(), ...[...children.values()].map((c) => c.stop())]);
	}

	// The Sandbox goes before the commit: a crash in between leaves a listed Thread whose
	// retry finishes the job, never an orphan Sandbox. Pi has no way to remove a transcript.
	async delete(id: ConversationId): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await conversation.abort(context, { background: true });
		const children = (await subagentsOf(this.#harness, id)).map((s) => s.conversationId);
		for (const each of [id, ...children]) {
			const session = await this.#providerSession(each);
			if (session) await this.#providerSessions.forget(session);
		}
		const sandbox = await this.sandbox(id);
		if (sandbox) await this.#sandboxes.delete(sandbox);
		this.#idleSince.delete(id);
		await this.#harness.commit(async (tx) => {
			const list = await tx.doc(ThreadList);
			list.items = list.items.filter((t) => t.id !== id);
			for (const each of [id, ...children]) await tx.retireDoc(ThreadSandbox, each);
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

	// Returns whether the Sandbox was running and is now paused; "busy" while a subagent works in it.
	async #sleep(id: ConversationId, stillIdle: () => boolean): Promise<boolean | "busy"> {
		const children = (await subagentsOf(this.#harness, id)).map((s) => s.conversationId);
		for (const child of children) if ((await this.#harness.snapshot(LiveDoc, child, context))?.run) return "busy";
		for (const each of [id, ...children]) {
			const session = await this.#providerSession(each);
			if (session && stillIdle()) await this.#providerSessions.release(session);
		}
		const name = await this.sandbox(id);
		if (!name || (await this.#sandboxes.state(name)) !== "running" || !stillIdle()) return false;
		await this.#sandboxes.pause(name, "idle");
		return true;
	}

	// The worker is full: the Thread idle the longest gives up its Sandbox before its idle pause.
	async #makeRoom(): Promise<boolean> {
		for (const [id] of [...this.#idleSince].sort((a, b) => a[1] - b[1])) {
			const paused = await this.#sleep(id, () => this.#idleSince.has(id)).then((r) => r === true).catch((e) => {
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

	// A Thread's conversation, or by name its subagent's.
	async #conversation(id: ConversationId, name: string | undefined) {
		const thread = await this.#known(id);
		if (!thread || name === undefined) return thread;
		const child = (await subagentsOf(this.#harness, id)).find((s) => s.name === name);
		return child && this.#harness.conversation(child.conversationId, context);
	}
}
