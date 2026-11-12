import { createHash, randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { deleteSession, type EffortLevel, getSessionInfo, type ModelInfo, type SDKMessage, type SDKResultMessage } from "@anthropic-ai/claude-agent-sdk";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import type { Api, AssistantMessage, ImageContent, JsonObject, JsonValue, Message, Model, SimpleStreamOptions, TextContent, ToolResultMessage, TranscriptContext } from "@earendil-works/pi-ai";
import { clampThinkingLevel, createProvider } from "@earendil-works/pi-ai/models";
import { type AssistantMessageEventStream, createAssistantMessageEventStream } from "@earendil-works/pi-ai/utils/event-stream";
import { getCurrentSystemPrompt, getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { ClaudeSession, ExtraUsage, offeredModels, TOOL_SERVER, type ToolDeclaration, type UserContent } from "./claude-session.ts";

export const CLAUDE_PLAN = "claude-plan";
const MODELS: Pick<ModelInfo, "value" | "displayName">[] = [
	{ value: "opus", displayName: "Opus" },
	{ value: "sonnet", displayName: "Sonnet" },
	{ value: "haiku", displayName: "Haiku" },
];
const EFFORTS: readonly string[] = ["low", "medium", "high", "xhigh", "max"] satisfies EffortLevel[];
const RUNTIME =
	"You are running in Dorf, a self-hosted coding agent, through Claude Code. Your tools run in the Thread's own Linux machine. Claude Code's slash commands, settings and built-in tools do not exist here: the owner picks the model when a Thread starts and the thinking level in Dorf's page.";
const TOOL_PREFIX = `mcp__${TOOL_SERVER}__`;

type Role = "user" | "assistant" | "toolResult";
type Group = { role: Role; hash: string; start: number; end: number };
type Saved = { claude: string; hashes: string[] };
type Thread = { saved?: Saved; session?: ClaudeSession; launch?: string; effort?: EffortLevel };

// Array.isArray leaves JsonValue's readonly arrays in the other branch.
const isList = (v: JsonValue): v is readonly JsonValue[] => Array.isArray(v);
function canonical(v: JsonValue): string {
	if (isList(v)) return `[${v.map(canonical).join(",")}]`;
	// oxlint-disable-next-line anti-slop/no-runtime-typeof -- v is parsed JSON already; typeof tells its objects from its primitives
	if (v === null || typeof v !== "object") return JSON.stringify(v) ?? "null";
	return `{${Object.keys(v)
		.sort()
		.filter((k) => v[k] !== undefined)
		.map((k) => `${JSON.stringify(k)}:${canonical(v[k])}`)
		.join(",")}}`;
}
const hash = (v: JsonValue) => createHash("sha256").update(canonical(v)).digest("hex");

const blocks = (content: string | (TextContent | ImageContent)[]) => (Array.isArray(content) ? content : [{ type: "text" as const, text: content }]);

// Thinking is display only and not sent back to Claude Code, so it does not count.
function visible(m: Exclude<Message, { role: "system" }>): JsonValue {
	if (m.role === "assistant") return m.content.filter((c) => c.type !== "thinking").map((c) => (c.type === "toolCall" ? { id: c.id, name: c.name, arguments: c.arguments } : c.text));
	const content = blocks(m.content).map((c) => (c.type === "text" ? c.text : { image: c.mimeType, data: hash(c.data) }));
	return m.role === "toolResult" ? { id: m.toolCallId, name: m.toolName, isError: m.isError, content } : content;
}

// A run of user messages, or of tool results, is one group: Claude Code takes each run as one message.
function groups(messages: Message[]): Group[] {
	const out: Group[] = [];
	for (let i = 0; i < messages.length; ) {
		const first = messages[i];
		if (first.role === "system") {
			i++;
			continue;
		}
		const start = i;
		const members = [visible(first)];
		for (i++; first.role !== "assistant" && messages[i]?.role === first.role; i++) members.push(visible(messages[i] as typeof first));
		out.push({ role: first.role, hash: hash({ role: first.role, members }), start, end: i });
	}
	return out;
}

function claudeBlocks(content: string | (TextContent | ImageContent)[]): UserContent {
	return blocks(content).map((c) =>
		c.type === "text" ? { type: "text", text: c.text } : { type: "image", source: { type: "base64", media_type: c.mimeType as "image/png", data: c.data } },
	);
}

function tagged(m: Exclude<Message, { role: "system" }>): UserContent {
	if (m.role === "user") return [{ type: "text", text: "<user>" }, ...claudeBlocks(m.content), { type: "text", text: "</user>" }];
	if (m.role === "toolResult") {
		const text = blocks(m.content).flatMap((c) => (c.type === "text" ? [c.text] : [])).join("\n");
		const result = JSON.stringify({ id: m.toolCallId, name: m.toolName, isError: m.isError, output: text });
		return [{ type: "text", text: `<tool-result>\n${result}\n</tool-result>` }, ...claudeBlocks(m.content.filter((c) => c.type === "image"))];
	}
	const parts = m.content.flatMap((c) =>
		c.type === "text" ? [c.text] : c.type === "toolCall" ? [`<tool-call>\n${JSON.stringify({ id: c.id, name: c.name, input: c.arguments })}\n</tool-call>`] : [],
	);
	return [{ type: "text", text: `<assistant>\n${parts.join("\n")}\n</assistant>` }];
}

// What Claude Code has not seen goes in a checkpoint, except the trailing user messages, which it answers.
function catchUp(messages: Message[], missed: Group[]): UserContent {
	const last = missed.findLastIndex((g) => g.role !== "user");
	const of = (gs: Group[]) => gs.flatMap((g) => messages.slice(g.start, g.end)) as Exclude<Message, { role: "system" }>[];
	const out: UserContent = [];
	if (last >= 0) {
		out.push({
			type: "text",
			text: "<conversation-checkpoint>\nThe following is conversation history from Dorf that you have not seen. Treat it as prior conversation, not a new request.",
		});
		for (const m of of(missed.slice(0, last + 1))) out.push(...tagged(m));
		out.push({ type: "text", text: "</conversation-checkpoint>" });
	}
	const latest = of(missed.slice(last + 1));
	for (const m of latest) out.push(...claudeBlocks((m as { content: string | (TextContent | ImageContent)[] }).content));
	if (!latest.length) out.push({ type: "text", text: "Continue the task." });
	return out;
}

type Part = (TextContent | ImageContent)[];
const toolParts = (content: Part) => content.map((c) => (c.type === "text" ? { type: "text" as const, text: c.text } : { type: "image" as const, data: c.data, mimeType: c.mimeType }));

const toolResult = (r: ToolResultMessage): CallToolResult => ({
	isError: r.isError,
	content: r.content.length ? toolParts(r.content) : [{ type: "text" as const, text: "(no output)" }],
});

class Reply {
	readonly message: AssistantMessage;
	readonly #stream: AssistantMessageEventStream;
	readonly #blocks = new Map<string, number>();
	readonly #usage = new Map<string, { input: number; cacheRead: number; cacheWrite: number; output: number }>();
	#id = "";

	constructor(stream: AssistantMessageEventStream, model: Model<Api>) {
		this.#stream = stream;
		this.message = {
			role: "assistant",
			content: [],
			api: model.api,
			provider: model.provider,
			model: model.id,
			usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
			stopReason: "stop",
			timestamp: Date.now(),
		};
		stream.push({ type: "start", partial: this.message });
	}

	// Claude Code reports one response's usage several times as it streams; the highest count is the response's.
	#count(id: string, u: { input_tokens?: number | null; cache_read_input_tokens?: number | null; cache_creation_input_tokens?: number | null; output_tokens?: number | null }) {
		const c = this.#usage.get(id) ?? { input: 0, cacheRead: 0, cacheWrite: 0, output: 0 };
		this.#usage.set(id, {
			input: Math.max(c.input, u.input_tokens ?? 0),
			cacheRead: Math.max(c.cacheRead, u.cache_read_input_tokens ?? 0),
			cacheWrite: Math.max(c.cacheWrite, u.cache_creation_input_tokens ?? 0),
			output: Math.max(c.output, u.output_tokens ?? 0),
		});
	}

	read(m: SDKMessage) {
		const partial = this.message;
		if (m.type === "stream_event") {
			const e = m.event;
			if (e.type === "message_start") {
				this.#id = e.message.id;
				partial.responseModel = e.message.model;
				this.#count(this.#id, e.message.usage);
			}
			if (e.type === "message_delta") this.#count(this.#id, e.usage);
			const key = `${this.#id}:${"index" in e ? e.index : ""}`;
			if (e.type === "content_block_start" && (e.content_block.type === "text" || e.content_block.type === "thinking")) {
				const text = e.content_block.type === "text";
				const index = partial.content.push(text ? { type: "text", text: "" } : { type: "thinking", thinking: "" }) - 1;
				this.#blocks.set(key, index);
				this.#stream.push({ type: text ? "text_start" : "thinking_start", contentIndex: index, partial });
			}
			const index = this.#blocks.get(key);
			if (e.type === "content_block_delta" && index !== undefined) {
				const block = partial.content[index];
				if (e.delta.type === "text_delta" && block.type === "text") {
					block.text += e.delta.text;
					this.#stream.push({ type: "text_delta", contentIndex: index, delta: e.delta.text, partial });
				}
				if (e.delta.type === "thinking_delta" && block.type === "thinking") {
					block.thinking += e.delta.thinking;
					this.#stream.push({ type: "thinking_delta", contentIndex: index, delta: e.delta.thinking, partial });
				}
			}
			if (e.type === "content_block_stop" && index !== undefined) {
				const block = partial.content[index];
				if (block.type === "text") this.#stream.push({ type: "text_end", contentIndex: index, content: block.text, partial });
				if (block.type === "thinking") this.#stream.push({ type: "thinking_end", contentIndex: index, content: block.thinking, partial });
			}
		}
		if (m.type === "assistant") {
			this.#count(m.message.id, m.message.usage);
			for (const b of m.message.content) {
				if (b.type !== "tool_use" || !b.name.startsWith(TOOL_PREFIX)) continue;
				const index = partial.content.push({ type: "toolCall", id: b.id, name: b.name.slice(TOOL_PREFIX.length), arguments: b.input as never }) - 1;
				this.#stream.push({ type: "toolcall_start", contentIndex: index, partial });
				this.#stream.push({ type: "toolcall_end", contentIndex: index, toolCall: partial.content[index] as never, partial });
			}
		}
	}

	finish(step: "tools" | SDKResultMessage) {
		const counts = [...this.#usage.values()];
		const last = counts.at(-1) ?? { input: 0, cacheRead: 0, cacheWrite: 0, output: 0 };
		const output = counts.reduce((sum, c) => sum + c.output, 0);
		Object.assign(this.message.usage, { ...last, output, totalTokens: last.input + last.cacheRead + last.cacheWrite + output });
		const reason = step === "tools" ? "toolUse" : step.stop_reason === "max_tokens" ? "length" : "stop";
		this.message.stopReason = reason;
		this.#stream.push({ type: "done", reason, message: this.message });
	}

	fail(error: Error, aborted: boolean) {
		this.message.stopReason = aborted ? "aborted" : "error";
		this.message.errorMessage = error.message;
		this.#stream.push({ type: "error", reason: this.message.stopReason, error: this.message });
	}
}

// Pi's thinking levels are Claude Code's effort levels; the ones a model lacks map to null.
function planModel(m: Pick<ModelInfo, "value" | "displayName" | "supportsEffort" | "supportedEffortLevels">) {
	const efforts: readonly string[] = m.supportedEffortLevels ?? (m.supportsEffort === false ? [] : ["low", "medium", "high"]);
	return {
		id: m.value,
		name: m.displayName,
		api: CLAUDE_PLAN,
		provider: CLAUDE_PLAN,
		baseUrl: "",
		input: ["text", "image"] as ("text" | "image")[],
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		reasoning: efforts.length > 0,
		thinkingLevelMap: Object.fromEntries(["off", "minimal", ...EFFORTS].map((level) => [level, efforts.includes(level) ? level : null])),
		contextWindow: 200_000,
		maxTokens: 32_000,
	};
}

const effortOf = (model: Model<Api>, reasoning: SimpleStreamOptions["reasoning"]) => {
	const level = reasoning && clampThinkingLevel(model, reasoning);
	return level && EFFORTS.includes(level) ? (level as EffortLevel) : undefined;
};

const NOT_CONNECTED = "Claude is not connected. Connect it under Settings.";
const BROKER_SYNC_MS = 90_000;
const REJECTED = "Claude rejected Dorf's connection. Connect it again under Settings.";

const failure = (step: "tools" | SDKResultMessage) =>
	step === "tools" || (step.subtype === "success" && !step.is_error)
		? undefined
		: step.subtype === "success" && step.result.startsWith("Failed to authenticate")
			? REJECTED
			: `Claude Code: ${step.subtype} ${step.subtype === "success" ? step.result : step.errors.join("; ")}`;

// Claude Code keeps its own copy of each Thread, so a request sends only the groups of Pi's history it has not seen.
export class ClaudePlan {
	readonly #cwd: string;
	readonly #records: string;
	readonly #executable: string | undefined;
	readonly #threads = new Map<string, Thread>();
	readonly #locks = new Map<string, Promise<unknown>>();
	#refusedUntil = 0;
	readonly provider;

	readonly #connected: (() => Promise<string | null>) | undefined;

	// `connected` says when the owner connected the plan, if they did, so a Thread fails at once instead of after Claude
	// Code's retries of a request without a token.
	constructor(dataDir: string, executable?: string, connected?: () => Promise<string | null>) {
		this.#connected = connected;
		this.#cwd = dataDir;
		this.#records = `${dataDir}/sessions`;
		this.#executable = executable;
		mkdirSync(this.#records, { recursive: true });
		const stream = (model: Model<Api>, context: TranscriptContext, options?: SimpleStreamOptions) => this.#stream(model, context, options);
		this.provider = createProvider({
			id: CLAUDE_PLAN,
			name: "Claude plan",
			auth: { apiKey: { name: "Claude Code login", resolve: async () => ({ auth: {} }) } },
			models: MODELS.map(planModel),
			fetchModels: async () => (await offeredModels(executable, dataDir)).filter((m) => m.value !== "default").map(planModel),
			api: { stream, streamSimple: stream },
		});
	}

	// Stops the Thread's Claude Code process; the next request resumes its saved session.
	release(sessionId: string): Promise<void> {
		return this.#serial(sessionId, async () => {
			const thread = this.#threads.get(sessionId);
			if (thread) await this.#stop(thread);
		});
	}

	forget(sessionId: string): Promise<void> {
		return this.#serial(sessionId, async () => {
			const thread = this.#threads.get(sessionId) ?? { saved: this.#load(sessionId) };
			this.#threads.delete(sessionId);
			await this.#stop(thread);
			rmSync(this.#file(sessionId), { force: true });
			if (thread.saved) await this.#deleteTranscript(thread.saved.claude);
		});
	}

	#serial<T>(sessionId: string, work: () => Promise<T>): Promise<T> {
		const run = (this.#locks.get(sessionId) ?? Promise.resolve()).then(work, work);
		const tail = run.catch(() => {});
		this.#locks.set(sessionId, tail);
		tail.then(() => this.#locks.get(sessionId) === tail && this.#locks.delete(sessionId));
		return run;
	}

	#file(sessionId: string) {
		if (!/^[\w-]+$/.test(sessionId)) throw new Error(`bad session id ${sessionId}`);
		return `${this.#records}/${sessionId}.json`;
	}

	#load(sessionId: string): Saved | undefined {
		try {
			return JSON.parse(readFileSync(this.#file(sessionId), "utf8"));
		} catch {
			return undefined;
		}
	}

	#save(sessionId: string, thread: Thread, saved: Saved) {
		thread.saved = saved;
		writeFileSync(this.#file(sessionId), JSON.stringify(saved));
	}

	#deleteTranscript(claude: string) {
		// Already gone is fine; a transcript removed without its subagent folder is not, and a retry cannot tell.
		return deleteSession(claude, { dir: this.#cwd }).catch((e: Error) => {
			if (!/^Session \S+ not found/.test(e.message)) console.error(`claude-plan: deleting session ${claude}: ${e.message}`);
		});
	}

	async #stop(thread: Thread) {
		const session = thread.session;
		thread.session = undefined;
		await session?.stop();
	}

	#stream(model: Model<Api>, context: TranscriptContext, options?: SimpleStreamOptions): AssistantMessageEventStream {
		const stream = createAssistantMessageEventStream();
		const reply = new Reply(stream, model);
		const tools = getCurrentTools(context.messages).map((t): ToolDeclaration => ({ name: t.name, description: t.description, parameters: t.parameters as JsonObject }));
		const system = getCurrentSystemPrompt(context.messages);
		const sessionId = options?.sessionId;
		const signal = options?.signal;
		const effort = effortOf(model, options?.reasoning);
		if (effort) reply.message.providerThinkingLevel = effort;
		const work = (async () => {
			if (Date.now() < this.#refusedUntil)
				throw new Error(`Claude plan requests are refused until ${new Date(this.#refusedUntil).toISOString()}: the plan's extra usage was drawn`);
			const connected = this.#connected && (await this.#connected());
			if (connected === null) throw new Error(NOT_CONNECTED);
			// The broker reads a new token when the kubelet next syncs its Secret, which takes up to about a minute.
			const fresh = connected ? Date.parse(connected) + BROKER_SYNC_MS - Date.now() : 0;
			if (fresh > 0) await new Promise((r) => (setTimeout(r, fresh), signal?.addEventListener("abort", r, { once: true })));
			signal?.throwIfAborted();
			// Requests without tools, like Pi's compaction summaries, stand alone and leave the Thread's session alone.
			return sessionId && tools.length
				? this.#serial(sessionId, () => this.#threadStep(model, effort, context.messages, tools, system, sessionId, reply, signal))
				: this.#oneShot(model, effort, context.messages, system, reply, signal);
		})();
		work.catch((e) => {
			if (e instanceof ExtraUsage) this.#refusedUntil = e.until;
			reply.fail(e, Boolean(signal?.aborted));
		});
		return stream;
	}

	async #oneShot(model: Model<Api>, effort: EffortLevel | undefined, messages: Message[], system: string, reply: Reply, signal?: AbortSignal) {
		const session = await ClaudeSession.open({
			executable: this.#executable,
			cwd: this.#cwd,
			model: model.id,
			effort,
			system: system || "Return only the requested output.",
			tools: [],
			persist: false,
			sessionId: randomUUID(),
		});
		const interrupt = () => void session.interrupt();
		signal?.addEventListener("abort", interrupt, { once: true });
		try {
			signal?.throwIfAborted();
			session.begin(catchUp(messages, groups(messages)));
			const step = await session.readStep((m) => reply.read(m));
			signal?.throwIfAborted();
			const failed = failure(step);
			if (failed) throw new Error(failed);
			reply.finish(step);
		} finally {
			signal?.removeEventListener("abort", interrupt);
			await session.stop();
		}
	}

	async #threadStep(model: Model<Api>, effort: EffortLevel | undefined, messages: Message[], tools: ToolDeclaration[], system: string, sessionId: string, reply: Reply, signal?: AbortSignal) {
		const thread = this.#threads.get(sessionId) ?? { saved: this.#load(sessionId) };
		this.#threads.set(sessionId, thread);
		const launch = hash({ model: model.id, system, tools });
		if (thread.session && (thread.session.ended || thread.launch !== launch)) await this.#stop(thread);
		// A session that refuses the new level is relaunched with it.
		if (thread.session && thread.effort !== effort) {
			await thread.session.setEffort(effort);
			thread.effort = effort;
		}

		const all = groups(messages);
		const sent = thread.saved?.hashes ?? [];
		let seen = 0;
		while (seen < sent.length && seen < all.length && sent[seen] === all[seen].hash) seen++;
		let resume = sent.length > 0 && seen === sent.length;
		if (sent.length && !resume) console.error(`claude-plan: Pi's history changed at group ${seen} of ${sent.length}, likely a compaction; starting a new Claude Code session`);
		let missed = resume ? all.slice(seen) : all;

		const [answered, ...after] = missed;
		const inTurn = answered?.role === "toolResult" && after.every((g) => g.role === "user");
		const results = inTurn ? (messages.slice(answered.start, answered.end) as ToolResultMessage[]) : [];
		const steer = inTurn ? (after.flatMap((g) => messages.slice(g.start, g.end)) as Extract<Message, { role: "user" }>[]) : [];
		const live = thread.session;
		let begun = false;
		const interrupt = () => void thread.session?.interrupt();
		signal?.addEventListener("abort", interrupt, { once: true });
		try {
			if (resume && live?.waiting && results.length && results.every((r) => live.pending.has(r.toolCallId))) {
				if (steer.length) live.steer(steer.flatMap((m) => claudeBlocks(m.content)));
				for (const r of results) live.pending.resolve(r.toolCallId, toolResult(r));
			} else {
				if (live && (live.waiting || !resume)) await this.#stop(thread);
				if (!thread.session) {
					if (resume && !(await getSessionInfo(thread.saved!.claude, { dir: this.#cwd }))) {
						resume = false;
						missed = all;
					}
					if (!resume) {
						if (thread.saved) await this.#deleteTranscript(thread.saved.claude);
						this.#save(sessionId, thread, { claude: randomUUID(), hashes: [] });
					}
					const claude = thread.saved!.claude;
					thread.session = await ClaudeSession.open({
						executable: this.#executable,
						cwd: this.#cwd,
						model: model.id,
						effort,
						system: { type: "preset", preset: "claude_code", append: `${RUNTIME}\n\n${system}` },
						tools,
						persist: true,
						...(resume ? { resume: claude } : { sessionId: claude }),
					});
					thread.launch = launch;
					thread.effort = effort;
				}
				signal?.throwIfAborted();
				thread.session.begin(catchUp(messages, missed));
			}
			begun = true;
			const step = await thread.session!.readStep((m) => reply.read(m));
			signal?.throwIfAborted();
			const failed = failure(step);
			if (failed) throw new Error(failed);
			reply.finish(step);
			this.#save(sessionId, thread, { claude: thread.saved!.claude, hashes: [...all.map((g) => g.hash), groups([reply.message])[0]?.hash ?? ""] });
		} catch (e) {
			// Claude Code already holds what was sent, so a retry of the same history only asks it to continue.
			await this.#stop(thread);
			if (begun) this.#save(sessionId, thread, { claude: thread.saved!.claude, hashes: all.map((g) => g.hash) });
			throw e;
		} finally {
			signal?.removeEventListener("abort", interrupt);
		}
	}
}
