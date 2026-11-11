import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { type AgentChange, type Conversation, type ConversationId, ConversationTranscript, type ConversationView, type EntryId, type ModelRef, type Harness, type HarnessOptions, LiveDoc, ProviderDoc } from "@earendil-works/pi-durable";
import type { ModelThinkingLevel } from "@earendil-works/pi-ai";
import type { UserInput } from "@earendil-works/pi-durable";
import { REPO } from "./github.ts";
import { pauseWhenIdle } from "./idle.ts";
import type { Sandboxes } from "./sandbox.ts";
import { placeStopped, Subagents, subagentsOf } from "./subagent.ts";
import { ThreadList, type ThreadSummary } from "./thread-list.ts";
import { ThreadSandbox } from "./thread-sandbox.ts";
import type { threadStart } from "./thread-start.ts";

export type ThreadAgent = { model?: ModelRef; thinkingLevel: ModelThinkingLevel };
export type SubagentView = { name: string; model?: ModelRef; working: boolean };
type FollowEvent = { entries?: ConversationView["entries"]; earlier?: boolean; live?: unknown; queued?: Queued[]; agent?: ThreadAgent; subagents?: SubagentView[] };

// Entries per page: the first window, each fill of a gap, each "load earlier".
const PAGE = 50;

export type { ThreadSummary };

// A repeat of a creation the agent already committed, found by the page's request id.
class Created extends Error {
	readonly id: ConversationId;
	constructor(id: ConversationId) {
		super(`Thread ${id} exists`);
		this.id = id;
	}
}

// Sent to a Thread whose first message is not in yet.
export class Starting extends Error {
	constructor() {
		super("the Thread is still starting; send once its first message is in");
	}
}

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
// Messages sent while the Thread works wait in Pi's inbox until the current step ends; the page shows them meanwhile.
// Only the user's own messages: an input placed as another entry, such as a subagent's report, shows once it lands.
// Images are counted, not sent: the live state goes out with each token, and they show once the message lands.
type Inbox = { items?: { mode: string; entry?: unknown; content?: string | { type: string; text?: string }[] }[] };
type Queued = { text: string; images: number };
const queued = (docs: Record<string, unknown>): Queued[] =>
	((docs["pi.inbox"] as Inbox | undefined)?.items ?? []).flatMap((item) => {
		if (item.mode === "write" || item.entry !== undefined || item.content === undefined) return [];
		if (typeof item.content === "string") return [{ text: item.content, images: 0 }];
		const text = item.content.flatMap((b) => (b.type === "text" && b.text ? [b.text] : [])).join("");
		return [{ text, images: item.content.filter((b) => b.type === "image").length }];
	});

export type ProviderSessions = { release(sessionId: string): Promise<void>; forget(sessionId: string): Promise<void> };

export class Threads {
	readonly #harness: Harness;
	readonly #sandboxes: Sandboxes;
	readonly #agent: AgentChange;
	readonly #idleMs: number;
	readonly #providerSessions: ProviderSessions;
	readonly #start: ReturnType<typeof threadStart>["task"];
	readonly #idleSince = new Map<ConversationId, number>();
	readonly #idle = new Map<ConversationId, { now(): void }>();

	constructor(harness: Harness, sandboxes: Sandboxes, start: ReturnType<typeof threadStart>["task"], agent: AgentChange, idleMs: number, providerSessions: ProviderSessions) {
		this.#start = start;
		this.#harness = harness;
		this.#sandboxes = sandboxes;
		this.#agent = agent;
		this.#idleMs = idleMs;
		this.#providerSessions = providerSessions;
		sandboxes.makeRoom = () => this.#makeRoom();
	}

	// An archive the agent stopped short of finishes now. The list exists before the page follows it.
	async open(): Promise<void> {
		await this.#harness.commit((tx) => tx.doc(ThreadList), context);
		for (const thread of await this.list()) {
			await this.#watchIdle(thread.id);
			if (thread.archived) this.#idle.get(thread.id)?.now();
		}
	}

	async list(): Promise<ThreadSummary[]> {
		return (await this.#harness.snapshot(ThreadList, context))?.items ?? [];
	}

	// The list now, then again whenever a Thread is added, archived, deleted, starts working or stops.
	async followList(send: (event: { threads: readonly ThreadSummary[] }) => void): Promise<(() => Promise<unknown>) | undefined> {
		const watch = await this.#harness.watchDoc(ThreadList, context);
		if (!watch) return undefined;
		send({ threads: watch.value.items });
		watch.start(async (call) => {
			if (call.kind === "value") send({ threads: call.value.items });
		});
		return () => watch.stop();
	}

	// The owner looked at the Thread's last run.
	async seen(id: ConversationId): Promise<boolean> {
		const thread = (await this.list()).find((t) => t.id === id);
		if (!thread) return false;
		if (thread.attention)
			await this.#harness.commit(async (tx) => {
				delete (await tx.doc(ThreadList)).items.find((t) => t.id === id)?.attention;
			}, context);
		return true;
	}

	// The list follows the Thread's run, read in the same commit so the newest state wins. A run that ends asks for the
	// owner's look unless the owner stopped it or archived the Thread.
	async #mark(id: ConversationId): Promise<void> {
		await this.#harness.commit(async (tx) => {
			const thread = (await tx.doc(ThreadList)).items.find((t) => t.id === id);
			if (!thread) return;
			const live = await tx.doc(LiveDoc, id);
			if (live.run) {
				if (thread.working) return;
				thread.working = true;
				thread.startedAt = new Date().toISOString();
				delete thread.attention;
				return;
			}
			if (!thread.working) return;
			delete thread.working;
			thread.endedAt = new Date().toISOString();
			const failure = live.runFailure?.reason;
			if (!thread.archived && failure !== "aborted") thread.attention = failure ? "failed" : "done";
		}, context);
	}

	async sandbox(id: ConversationId): Promise<string | undefined> {
		return (await this.#harness.snapshot(ThreadSandbox, id, context))?.name;
	}

	// Answers once the Thread is committed: a background task then starts its Sandbox, clones the repository and sends
	// the first message, waiting for a worker with room as long as it takes. The Thread exists before its Sandbox, so no
	// Sandbox is ever left without a Thread. A repeat with the same `request` returns the Thread the first one made.
	async create(content: UserInput, title: string, repo: string | undefined, choice: { model: ModelRef; thinkingLevel: ModelThinkingLevel }, request?: string): Promise<{ id: ConversationId; sandbox: string }> {
		if (repo !== undefined && !REPO.test(repo)) throw new RangeError(`not a GitHub repository: ${repo}`);
		const sandbox = this.#sandboxes.newName();
		const workspace = this.#agent.cwd ?? "/";
		const cwd = repo ? `${workspace}/${repo.split("/")[1]}` : workspace;
		const agent = { ...this.#agent, ...choice, cwd };
		let id: ConversationId;
		try {
			({ id } = await this.#harness.createConversation(
				{
					ownership: { kind: "ownerless" },
					agent,
					init: async (tx, id) => {
						const list = await tx.doc(ThreadList);
						const first = request === undefined ? undefined : list.items.find((t) => t.request === request);
						if (first) throw new Created(first.id);
						Object.assign(await tx.doc(ThreadSandbox, id), { name: sandbox, ...(repo ? { repo } : {}) });
						await tx.doc(Subagents, id);
						list.items.push({ id, title: title.slice(0, 80), createdAt: new Date().toISOString(), ...(repo ? { repo } : {}), starting: true, ...(request ? { request } : {}) });
						await tx.createTask(this.#start, { sandbox, repo, workspace, cwd, content }, { ownership: { kind: "conversation" }, conversationId: id, background: true });
					},
				},
				context,
			));
		} catch (e) {
			if (!(e instanceof Created)) throw e;
			return { id: e.id, sandbox: (await this.sandbox(e.id))! };
		}
		await this.#watchIdle(id);
		return { id, sandbox };
	}

	// The Threads whose Sandboxes run, and so hold a worker's room.
	async holders(): Promise<ConversationId[]> {
		const threads = await this.list();
		const running = await Promise.all(
			threads.map(async (t) => {
				const name = await this.sandbox(t.id);
				return name !== undefined && (await this.#sandboxes.state(name).catch(() => undefined)) === "running";
			}),
		);
		return threads.filter((_, i) => running[i]).map((t) => t.id);
	}

	async #starting(id: ConversationId): Promise<boolean> {
		return Boolean((await this.list()).find((t) => t.id === id)?.starting);
	}

	// Stops the Thread's work and its subagents', but not its start: only deleting the Thread ends that.
	async #stop(conversation: Conversation): Promise<void> {
		await conversation.abort(context, (await this.#starting(conversation.id)) ? undefined : { background: true });
		await placeStopped(conversation, context);
	}

	// A message brings an archived Thread back.
	async send(id: ConversationId, content: UserInput): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		if (await this.#starting(id)) throw new Starting();
		await this.#archive(id, false);
		await this.#harness.commit(async (tx) => {
			const thread = (await tx.doc(ThreadList)).items.find((t) => t.id === id);
			if (thread) thread.askedAt = new Date().toISOString();
		}, context);
		await conversation.submit({ type: "input", content, whenBusy: "steer" }, context);
		return true;
	}

	// As in Amp: an archived Thread leaves the list, its work and its subagents' stop, and its Sandbox pauses at once.
	async archive(id: ConversationId): Promise<boolean> {
		const conversation = await this.#known(id);
		if (!conversation) return false;
		await this.#archive(id, true);
		await this.#stop(conversation);
		this.#idle.get(id)?.now();
		return true;
	}

	async unarchive(id: ConversationId): Promise<boolean> {
		if (!(await this.#known(id))) return false;
		await this.#archive(id, false);
		return true;
	}

	async #archive(id: ConversationId, archived: boolean): Promise<void> {
		if ((await this.list()).find((t) => t.id === id)?.archived === (archived || undefined)) return;
		await this.#harness.commit(async (tx) => {
			const thread = (await tx.doc(ThreadList)).items.find((t) => t.id === id);
			if (thread && archived) thread.archived = true;
			else if (thread) delete thread.archived;
		}, context);
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
		// Stop ends everything the Thread started: its run, its subagents' work and their reporters.
		await this.#stop(conversation);
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
		send({ entries, ...(after === undefined ? { earlier } : {}), live: watch.value.docs["pi.live"] ?? {}, queued: queued(watch.value.docs), agent: await agentOf(conversation), ...(withSubagents ? { subagents: subagents() } : {}) });
		watch.start(async (call) => {
			if (call.kind !== "value") return;
			const { entries } = await fresh(call.value);
			const agentChanged = call.value.docs["pi.agent"] !== agent;
			agent = call.value.docs["pi.agent"];
			send({ ...(entries.length ? { entries } : {}), live: call.value.docs["pi.live"] ?? {}, queued: queued(call.value.docs), ...(agentChanged ? { agent: await agentOf(conversation) } : {}) });
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
		this.#idle.delete(id);
		await this.#harness.commit(async (tx) => {
			const list = await tx.doc(ThreadList);
			list.items = list.items.filter((t) => t.id !== id);
			for (const each of [id, ...children]) await tx.retireDoc(ThreadSandbox, each);
		}, context);
		return true;
	}

	async #watchIdle(id: ConversationId): Promise<void> {
		const idle = await pauseWhenIdle(
			this.#harness,
			id,
			this.#idleMs,
			(stillIdle) => this.#sleep(id, stillIdle),
			(running) => {
				if (running) this.#idleSince.delete(id);
				else this.#idleSince.set(id, Date.now());
				this.#mark(id).catch((e) => console.error(`mark Thread ${id}: ${e.message}`));
			},
		);
		if (idle) this.#idle.set(id, idle);
	}

	// Returns whether the Sandbox was running and is now paused; "busy" while a subagent works in it. A Sandbox still
	// being started waits for its first message.
	async #sleep(id: ConversationId, stillIdle: () => boolean): Promise<boolean | "busy"> {
		if (await this.#starting(id)) return false;
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
