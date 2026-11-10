import { createHash, randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { deleteSession, getSessionInfo, type SDKMessage, type SDKResultMessage } from "@anthropic-ai/claude-agent-sdk";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import type { Api, AssistantMessage, ImageContent, Message, Model, SimpleStreamOptions, TextContent, ToolResultMessage, TranscriptContext } from "@earendil-works/pi-ai";
import { createProvider } from "@earendil-works/pi-ai/models";
import { type AssistantMessageEventStream, createAssistantMessageEventStream } from "@earendil-works/pi-ai/utils/event-stream";
import { getCurrentSystemPrompt, getCurrentTools } from "@earendil-works/pi-ai/utils/transcript";
import { ClaudeSession, ExtraUsage, TOOL_SERVER, type ToolDeclaration, type UserContent } from "./claude-session.ts";

export const CLAUDE_PLAN = "claude-plan";
const MODELS = ["opus", "sonnet", "haiku"];
const RUNTIME = "You are running in Dorf through Claude Code. Your tools run in the Thread's own Linux machine.";
const TOOL_PREFIX = `mcp__${TOOL_SERVER}__`;

type Role = "user" | "assistant" | "toolResult";
type Group = { role: Role; hash: string; start: number; end: number };
type Saved = { claude: string; hashes: string[] };
type Thread = { saved?: Saved; session?: ClaudeSession; launch?: string };

const canonical = (v: unknown): string =>
	Array.isArray(v)
		? `[${v.map(canonical).join(",")}]`
		: v !== null && typeof v === "object"
			? `{${Object.keys(v)
					.sort()
					.filter((k) => (v as Record<string, unknown>)[k] !== undefined)
					.map((k) => `${JSON.stringify(k)}:${canonical((v as Record<string, unknown>)[k])}`)
					.join(",")}}`
			: (JSON.stringify(v) ?? "null");
const hash = (v: unknown) => createHash("sha256").update(canonical(v)).digest("hex");

const blocks = (content: string | (TextContent | ImageContent)[]) => (typeof content === "string" ? [{ type: "text" as const, text: content }] : content);

// Thinking is display only and not sent back to Claude Code, so it does not count.
function visible(m: Exclude<Message, { role: "system" }>): unknown {
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

const toolResult = (r: ToolResultMessage): CallToolResult => ({
	isError: r.isError,
	content: r.content.length
		? r.content.map((c) => (c.type === "text" ? { type: "text" as const, text: c.text } : { type: "image" as const, data: c.data, mimeType: c.mimeType }))
		: [{ type: "text", text: "(no output)" }],
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

	fail(error: unknown, aborted: boolean) {
		this.message.stopReason = aborted ? "aborted" : "error";
		this.message.errorMessage = (error as Error).message;
		this.#stream.push({ type: "error", reason: this.message.stopReason, error: this.message });
	}
}

const failure = (step: "tools" | SDKResultMessage) =>
	step !== "tools" && (step.subtype !== "success" || step.is_error) ? `Claude Code: ${step.subtype} ${step.subtype === "success" ? step.result : step.errors.join("; ")}` : undefined;

// Claude Code keeps its own copy of each Thread, so a request sends only the groups of Pi's history it has not seen.
export class ClaudePlan {
	readonly #cwd: string;
	readonly #records: string;
	readonly #executable: string | undefined;
	readonly #threads = new Map<string, Thread>();
	readonly #locks = new Map<string, Promise<unknown>>();
	#refusedUntil = 0;
	readonly provider;

	constructor(dataDir: string, executable?: string) {
		this.#cwd = dataDir;
		this.#records = `${dataDir}/sessions`;
		this.#executable = executable;
		mkdirSync(this.#records, { recursive: true });
		const stream = (model: Model<Api>, context: TranscriptContext, options?: SimpleStreamOptions) => this.#stream(model, context, options);
		this.provider = createProvider({
			id: CLAUDE_PLAN,
			name: "Claude plan",
			auth: { apiKey: { name: "Claude Code login", resolve: async () => ({ auth: {} }) } },
			models: MODELS.map((id) => ({
				id,
				name: `Claude ${id} (plan)`,
				api: CLAUDE_PLAN,
				provider: CLAUDE_PLAN,
				baseUrl: "",
				input: ["text", "image"] as ("text" | "image")[],
				cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
				reasoning: true,
				contextWindow: 200_000,
				maxTokens: 32_000,
			})),
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
		return deleteSession(claude, { dir: this.#cwd }).catch(() => {});
	}

	async #stop(thread: Thread) {
		const session = thread.session;
		thread.session = undefined;
		await session?.stop();
	}

	#stream(model: Model<Api>, context: TranscriptContext, options?: SimpleStreamOptions): AssistantMessageEventStream {
		const stream = createAssistantMessageEventStream();
		const reply = new Reply(stream, model);
		const tools = getCurrentTools(context.messages).map((t): ToolDeclaration => ({ name: t.name, description: t.description, parameters: t.parameters }));
		const system = getCurrentSystemPrompt(context.messages);
		const sessionId = options?.sessionId;
		const signal = options?.signal;
		const work =
			Date.now() < this.#refusedUntil
				? Promise.reject(new Error(`Claude plan requests are refused until ${new Date(this.#refusedUntil).toISOString()}: the plan's extra usage was drawn`))
				: // Requests without tools, like Pi's compaction summaries, stand alone and leave the Thread's session alone.
					sessionId && tools.length
					? this.#serial(sessionId, () => this.#threadStep(model, context.messages, tools, system, sessionId, reply, signal))
					: this.#oneShot(model, context.messages, system, reply, signal);
		work.catch((e) => {
			if (e instanceof ExtraUsage) this.#refusedUntil = e.until;
			reply.fail(e, Boolean(signal?.aborted));
		});
		return stream;
	}

	async #oneShot(model: Model<Api>, messages: Message[], system: string, reply: Reply, signal?: AbortSignal) {
		const session = await ClaudeSession.open({
			executable: this.#executable,
			cwd: this.#cwd,
			model: model.id,
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

	async #threadStep(model: Model<Api>, messages: Message[], tools: ToolDeclaration[], system: string, sessionId: string, reply: Reply, signal?: AbortSignal) {
		const thread = this.#threads.get(sessionId) ?? { saved: this.#load(sessionId) };
		this.#threads.set(sessionId, thread);
		const launch = hash({ model: model.id, system, tools });
		if (thread.session && (thread.session.ended || thread.launch !== launch)) await this.#stop(thread);

		const all = groups(messages);
		const sent = thread.saved?.hashes ?? [];
		let seen = 0;
		while (seen < sent.length && seen < all.length && sent[seen] === all[seen].hash) seen++;
		let resume = sent.length > 0 && seen === sent.length;
		if (sent.length && !resume) console.error(`claude-plan: Pi's history changed at group ${seen} of ${sent.length}, likely a compaction; starting a new Claude Code session`);
		let missed = resume ? all.slice(seen) : all;

		const results = missed.length === 1 && missed[0].role === "toolResult" ? (messages.slice(missed[0].start, missed[0].end) as ToolResultMessage[]) : [];
		const live = thread.session;
		let begun = false;
		const interrupt = () => void thread.session?.interrupt();
		signal?.addEventListener("abort", interrupt, { once: true });
		try {
			if (resume && live?.waiting && results.length && results.every((r) => live.pending.has(r.toolCallId))) {
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
						system: { type: "preset", preset: "claude_code", append: `${RUNTIME}\n\n${system}` },
						tools,
						persist: true,
						...(resume ? { resume: claude } : { sessionId: claude }),
					});
					thread.launch = launch;
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
