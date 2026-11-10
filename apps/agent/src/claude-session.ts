import { type Options, query, type Query, type SDKMessage, type SDKResultMessage, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { CallToolRequestSchema, type CallToolResult, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";

export type UserContent = Exclude<SDKUserMessage["message"]["content"], string>[number][];
export type ToolDeclaration = { name: string; description: string; parameters: object };

export const TOOL_SERVER = "dorf";
const MIN_VERSION = [2, 1, 248];

// Claude Code features that would duplicate Pi's, or bill or behave apart from it.
const ISOLATION_ENV = {
	CLAUDE_CODE_DISABLE_CLAUDE_MDS: "1",
	CLAUDE_CODE_DISABLE_AUTO_MEMORY: "1",
	DISABLE_AUTO_COMPACT: "1",
	CLAUDE_CODE_ENABLE_AWAY_SUMMARY: "0",
	ENABLE_TOOL_SEARCH: "false",
	CLAUDE_CODE_DISABLE_ADVISOR_TOOL: "1",
	MAX_MCP_OUTPUT_TOKENS: "200000",
};
// An allowlist, so no API key, token or provider switch in the agent's environment reaches Claude Code.
const PASSED_ENV = ["PATH", "HOME", "LANG", "TERM", "TMPDIR", "USER"];
export const claudeEnv = () => Object.fromEntries(PASSED_ENV.flatMap((k) => (process.env[k] ? [[k, process.env[k]!]] : [])));

export type SessionStart = {
	executable?: string;
	cwd: string;
	model: string;
	system: Options["systemPrompt"];
	tools: ToolDeclaration[];
	persist: boolean;
} & ({ sessionId: string } | { resume: string });

const atLeast = (version: string, min: number[]) => {
	const v = version.split(/[-+]/)[0].split(".").map(Number);
	const i = min.findIndex((part, n) => (v[n] ?? 0) !== part);
	return i === -1 || (v[i] ?? 0) > min[i];
};

const STOP_TIMEOUT_MS = 10_000;

// Claude Code reports extra usage with the response that used it, so later requests must wait for the plan's reset.
export class ExtraUsage extends Error {
	readonly until: number;
	constructor(resetsAt?: number) {
		super("Claude Code drew on extra usage instead of the plan; Claude plan requests are refused until the plan resets");
		this.until = resetsAt ? (resetsAt < 1e12 ? resetsAt * 1000 : resetsAt) : Date.now() + 60 * 60_000;
	}
}

function extraUsage(m: SDKMessage): ExtraUsage | undefined {
	if (m.type !== "rate_limit_event") return undefined;
	const info = m.rate_limit_info;
	return info.isUsingOverage || info.overageInUse || info.rateLimitType === "overage" ? new ExtraUsage(info.resetsAt) : undefined;
}

// Claude Code runs tools by calling this server; each call waits until Pi has run the tool and resolves it.
class PendingTools {
	readonly #calls = new Map<string, PromiseWithResolvers<CallToolResult>>();
	#next: string | undefined;

	expect(id: string) {
		if (!this.#calls.has(id)) this.#calls.set(id, Promise.withResolvers());
		return this.#calls.get(id)!;
	}

	// Claude Code runs non-read-only tools one at a time, so the PreToolUse hook names the next handler's call.
	before(id: string) {
		this.expect(id);
		this.#next = id;
	}

	async call(): Promise<CallToolResult> {
		const id = this.#next;
		this.#next = undefined;
		if (!id) throw new Error("Claude Code called a tool without its PreToolUse id");
		const result = await this.expect(id).promise;
		this.#calls.delete(id);
		return result;
	}

	has(id: string) {
		return this.#calls.has(id);
	}

	resolve(id: string, result: CallToolResult) {
		const call = this.#calls.get(id);
		if (!call) throw new Error(`no waiting Claude Code tool call ${id}`);
		call.resolve(result);
	}

	// Only after an interrupt has drained the turn; earlier, a resolved handler starts another model call.
	clear() {
		for (const call of this.#calls.values()) call.resolve({ isError: true, content: [{ type: "text", text: "Cancelled." }] });
		this.#calls.clear();
		this.#next = undefined;
	}
}

function toolServer(tools: ToolDeclaration[], pending: PendingTools) {
	const server = new Server({ name: TOOL_SERVER, version: "1.0.0" }, { capabilities: { tools: {} } });
	const listed = tools.map((t) => ({
		name: t.name,
		description: t.description.slice(0, 2048),
		inputSchema: t.parameters as { type: "object" },
		annotations: { readOnlyHint: false },
		_meta: { "anthropic/alwaysLoad": true, "anthropic/maxResultSizeChars": 500_000 },
	}));
	server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: listed }));
	server.setRequestHandler(CallToolRequestSchema, () => pending.call());
	return { type: "sdk" as const, name: TOOL_SERVER, instance: server as never };
}

export class ClaudeSession {
	readonly id: string;
	readonly pending = new PendingTools();
	readonly #query: Query;
	readonly #input: ReadableStreamDefaultController<SDKUserMessage>;
	readonly #messages: SDKMessage[] = [];
	readonly #pump: Promise<void>;
	#wake: (() => void) | undefined;
	#turn: PromiseWithResolvers<void> | undefined;
	#ended = false;
	#failure: unknown;
	#stopping: Promise<void> | undefined;

	private constructor(start: SessionStart) {
		let input!: ReadableStreamDefaultController<SDKUserMessage>;
		const prompt = new ReadableStream<SDKUserMessage>({ start: (c) => void (input = c) });
		this.#input = input;
		this.id = "resume" in start ? start.resume : start.sessionId;
		this.#query = query({
			prompt,
			options: {
				pathToClaudeCodeExecutable: start.executable,
				env: { ...claudeEnv(), ...ISOLATION_ENV },
				cwd: start.cwd,
				model: start.model,
				systemPrompt: start.system,
				settingSources: [],
				settings: { disableAllHooks: true },
				strictMcpConfig: true,
				skills: [],
				tools: [],
				permissionMode: "dontAsk",
				allowedTools: [`mcp__${TOOL_SERVER}__*`],
				mcpServers: start.tools.length ? { [TOOL_SERVER]: toolServer(start.tools, this.pending) } : {},
				hooks: { PreToolUse: [{ hooks: [async (i) => (i.hook_event_name === "PreToolUse" && this.pending.before(i.tool_use_id), {})] }] },
				verbatimPrompts: true,
				thinking: { type: "adaptive", display: "summarized" },
				includePartialMessages: true,
				persistSession: start.persist,
				...("resume" in start ? { resume: start.resume } : { sessionId: start.sessionId }),
			},
		});
		this.#pump = this.#readAll();
	}

	// Every check runs before the first prompt, so a wrong login or model never costs a request.
	static async open(start: SessionStart): Promise<ClaudeSession> {
		const session = new ClaudeSession(start);
		try {
			const init = await session.#query.initializationResult();
			const account = init.account ?? {};
			const offered = init.models.map((m) => m.value);
			const problems = [
				account.apiProvider !== "firstParty" && `Claude Code uses ${account.apiProvider}, not a Claude plan`,
				account.apiKeySource && `Claude Code uses an API key (${account.apiKeySource}), not a Claude plan`,
				!account.subscriptionType && "Claude Code is not signed in to a Claude plan; connect Claude first",
				!atLeast(init.claude_code_version ?? "0", MIN_VERSION) && `Claude Code ${init.claude_code_version} is older than ${MIN_VERSION.join(".")}`,
				!offered.includes(start.model) && `Claude Code does not offer ${start.model}; it offers ${offered.join(", ")}`,
				start.tools.length > 0 &&
					(await session.#query.mcpServerStatus()).find((s) => s.name === TOOL_SERVER)?.status !== "connected" &&
					"Dorf's tool server did not connect to Claude Code",
			].filter(Boolean);
			if (problems.length) throw new Error(problems.join("; "));
		} catch (e) {
			session.#query.close();
			throw e;
		}
		return session;
	}

	get ended() {
		return this.#ended;
	}

	get waiting() {
		return Boolean(this.#turn);
	}

	async #readAll() {
		try {
			for await (const message of this.#query) {
				this.#messages.push(message);
				this.#wake?.();
				if (message.type === "result") this.#endTurn();
			}
		} catch (e) {
			this.#failure = e;
		} finally {
			this.#ended = true;
			this.#endTurn();
			this.#wake?.();
		}
	}

	#endTurn() {
		this.#turn?.resolve();
		this.#turn = undefined;
	}

	begin(content: UserContent) {
		if (this.#ended || this.#stopping) throw (this.#failure as Error) ?? new Error("Claude Code has stopped");
		if (this.#turn) throw new Error("Claude Code is still in a turn");
		this.#messages.length = 0;
		this.#turn = Promise.withResolvers();
		this.#input.enqueue({ type: "user", message: { role: "user", content }, parent_tool_use_id: null });
	}

	// One model response: ends when it called tools, which now wait on `pending`, or at the turn's result.
	async readStep(emit: (m: SDKMessage) => void): Promise<"tools" | SDKResultMessage> {
		let calledTools = false;
		for (;;) {
			const message = this.#messages.shift();
			if (!message) {
				if (this.#ended) throw (this.#failure as Error) ?? new Error("Claude Code stopped before it finished its reply");
				await new Promise<void>((r) => (this.#wake = r));
				continue;
			}
			const overage = extraUsage(message);
			if (overage) throw overage;
			if ("parent_tool_use_id" in message && message.parent_tool_use_id) continue;
			emit(message);
			if (message.type === "assistant") {
				for (const b of message.message.content) {
					if (b.type !== "tool_use" || !b.name.startsWith(`mcp__${TOOL_SERVER}__`)) continue;
					this.pending.expect(b.id);
					calledTools = true;
				}
			}
			if (calledTools && message.type === "stream_event" && message.event.type === "message_stop") return "tools";
			if (message.type === "result") return message;
		}
	}

	interrupt(): Promise<unknown> {
		return this.#turn ? this.#query.interrupt() : Promise.resolve();
	}

	// Interrupting first keeps a pending tool call from turning into a "declined" reply that costs a model call.
	stop(): Promise<void> {
		const drain = async () => {
			if (this.#turn && !this.#ended) {
				await this.#query.interrupt();
				await this.#turn?.promise;
			}
			this.pending.clear();
			this.#input.close();
			await this.#pump;
		};
		this.#stopping ??= Promise.race([drain(), new Promise<void>((r) => setTimeout(r, STOP_TIMEOUT_MS).unref())])
			.catch(() => {})
			.finally(() => this.#query.close());
		return this.#stopping;
	}
}
