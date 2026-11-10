import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import type { AssistantMessage } from "@earendil-works/pi-ai";
import {
	AgentDoc,
	AssistantEntry,
	type ConversationId,
	configure,
	defineDoc,
	defineEntry,
	defineExtension,
	defineTask,
	defineTool,
	type EntryId,
	type Extension,
	type Harness,
	LiveDoc,
	type TaskId,
} from "@earendil-works/pi-durable";
import { bool, obj, oneOf, opt, str } from "@earendil-works/pi-schema";
import type { Catalog } from "./models.ts";
import { ThreadSandbox } from "./thread-sandbox.ts";

// Pi's persistent background subagents (its example 22). Each subagent is a conversation of its own that remembers
// earlier messages, works in the Thread's Sandbox, and reports each answer back to the Thread as a follow-up.
export const SUBAGENTS_DOC = "dorf.subagents";
type Subagent = { conversationId: ConversationId; reported: EntryId[] };
// A subagent's answer in the Thread: the model reads its text, the page its author.
export const SubagentReport = defineEntry<{ name: string; failed?: true }>("dorf.subagent-report");
const Subagents = defineDoc<{ agents: Record<string, Subagent>; reporters: Record<string, TaskId> }>({
	checkpointWhen: () => false,
	kind: SUBAGENTS_DOC,
	version: 1,
	scope: "conversation",
	history: "latest",
	fork: "initial",
	initial: () => ({ agents: {}, reporters: {} }),
});

export async function subagentsOf(harness: Harness, id: ConversationId): Promise<{ name: string; conversationId: ConversationId }[]> {
	const doc = await harness.snapshot(Subagents, id, BACKGROUND_CONTEXT);
	return Object.entries(doc?.agents ?? {}).map(([name, a]) => ({ name, conversationId: a.conversationId }));
}

// Owns a subagent's conversation so it outlives the Thread's runs; the Thread's stop and idle waits end at it.
const Anchor = defineTask<null, { phase: "done" }, null>({
	name: "dorf.subagent-anchor",
	version: 1,
	initial: () => ({ phase: "done" }),
	phases: {
		done: (_anchor, runtime, taskContext) => runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), taskContext),
	},
	abort: (_anchor, runtime, taskContext) => runtime.commit(() => ({ status: "terminal", outcome: { status: "aborted" } }), taskContext),
});

const textOf = (message: AssistantMessage | undefined) => (message?.content ?? []).flatMap((c) => (c.type === "text" ? [c.text] : [])).join("");

// Delivers one message to a subagent and reports its answer to the Thread, once each across restarts.
type ReporterInput = { name: string; conversationId: ConversationId; message: string; followUp: boolean };
type ReporterState = { phase: "deliver" } | { phase: "report"; report?: string; failed?: true };
const Reporter = defineTask<ReporterInput, ReporterState, null>({
	name: "dorf.subagent-reporter",
	version: 1,
	initial: () => ({ phase: "deliver" }),
	phases: {
		deliver: async (reporter, runtime, taskContext) => {
			const { name, conversationId, message, followUp } = reporter.input;
			const subagent = (await runtime.conversation(conversationId, taskContext))!;
			const request = { type: "input", content: message, whenBusy: followUp ? "followUp" : "steer" } as const;
			const settled = await (await subagent.submit({ ...request, requestId: `subagent:${reporter.id}` }, taskContext)).wait(taskContext);
			await runtime.commit(async (tx) => {
				const next = (report?: string, failed?: true) => ({ status: "running", checkpoint: { phase: "report", report, ...(failed ? { failed } : {}) } }) as const;
				if (settled.status === "unanswered") return next(settled.reason === "aborted" ? undefined : `[subagent ${name} failed: ${settled.reason}]`, true);
				if (settled.type !== "input") return next();
				const agent = (await tx.doc(Subagents, runtime.conversationId)).agents[name]!;
				if (agent.reported.includes(settled.answer)) return next();
				agent.reported.push(settled.answer);
				const answer = (await tx.entry(AssistantEntry, settled.answer))?.model?.[0] as AssistantMessage;
				return next(`[subagent ${name} answered, no reply needed] ${textOf(answer)}`);
			}, taskContext);
		},
		report: async (reporter, runtime, taskContext) => {
			const { report, failed } = reporter.state.checkpoint;
			if (report !== undefined) {
				const thread = (await runtime.conversation(runtime.conversationId, taskContext))!;
				const entry = { kind: SubagentReport.kind, data: { name: reporter.input.name, ...(failed ? { failed } : {}) } };
				await thread.submit({ type: "input", content: report, entry, whenBusy: "followUp", requestId: `subagent-report:${reporter.id}` }, taskContext);
			}
			await runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), taskContext);
		},
	},
	abort: (_reporter, runtime, taskContext) => runtime.commit(() => ({ status: "terminal", outcome: { status: "aborted" } }), taskContext),
});

export function subagents(catalog: Catalog): Extension {
	const tool = defineTool({
		name: "subagent",
		description:
			"Manage persistent subagents that work in the background in this Thread's machine. Actions: spawn (name, message, " +
			"optional model: provider/model of another offered model, e.g. a stronger one for a hard question), send (name, " +
			"message; followUp: true queues it after the current answer instead of steering), stop (name: aborts its current " +
			"work), status (name, or all subagents without one). Each subagent remembers its earlier messages. Answers are " +
			"reported back to you when they arrive.",
		parameters: obj({
			action: oneOf(["spawn", "send", "stop", "status"]),
			name: opt(str()),
			message: opt(str()),
			model: opt(str()),
			followUp: opt(bool()),
		}),
		// Repeating an interrupted `stop` could stop newer work; the model can check with `status`.
		replay: "unsafe",
		execute: async (args, api, callContext) => {
			const { action, name, message, model, followUp } = args;
			const reply = (text: string, conversationId?: ConversationId) => ({
				output: [{ type: "text" as const, text }],
				...(conversationId === undefined || name === undefined ? {} : { details: { name, conversationId } }),
			});
			const registry = (await api.snapshot(Subagents, api.conversationId, callContext)) ?? { agents: {}, reporters: {} };

			if (action === "status") {
				const lines: string[] = [];
				for (const each of name === undefined ? Object.keys(registry.agents) : [name]) {
					const found = Object.hasOwn(registry.agents, each) ? registry.agents[each] : undefined;
					if (!found) continue;
					const busy = (await api.snapshot(LiveDoc, found.conversationId, callContext))?.run !== undefined;
					lines.push(`${each}: ${busy ? "working" : "idle"}`);
				}
				return reply(lines.length === 0 ? "No subagents." : lines.join("\n"));
			}
			if (name === undefined) return reply(`${action} needs a name.`);
			const agent = Object.hasOwn(registry.agents, name) ? registry.agents[name] : undefined;
			if (action !== "spawn" && agent === undefined) return reply(`No subagent named ${name}.`);
			if (action === "stop") {
				await (await api.conversation(agent!.conversationId, callContext))!.abort(callContext);
				return reply(`Stopped ${name}.`, agent!.conversationId);
			}
			if (message === undefined) return reply(`${action} needs a message.`);
			// A subagent runs on the Thread's model and thinking level, or on the model asked for at the nearest level.
			const thread = action === "spawn" ? await api.agent(callContext) : undefined;
			const choice = thread && (model === undefined ? { model: thread.model, thinkingLevel: thread.thinkingLevel } : catalog.near(model, thread.thinkingLevel));

			const result = await api.commit(async (tx) => {
				const state = await tx.doc(Subagents, api.conversationId);
				const background = { ownership: { kind: "conversation" }, background: true } as const;
				if (action === "spawn") {
					if (Object.hasOwn(state.agents, name)) return `${name} already exists; use send.`;
					const anchor = await tx.createTask(Anchor, null, background);
					const child = await tx.createConversation({ ownership: { kind: "task", taskId: anchor } });
					(await tx.doc(ThreadSandbox, child.id)).name = (await tx.doc(ThreadSandbox, api.conversationId)).name;
					const instructions = (await tx.doc(AgentDoc, api.conversationId)).instructions;
					await configure(tx, child.id, {
						extensions: { remove: [extension] },
						instructions: `${instructions ? `${instructions}\n\n` : ""}You are the subagent "${name}". Answer the main agent's requests.`,
						...choice,
					});
					state.agents[name] = { conversationId: child.id, reported: [] };
				}
				const conversationId = state.agents[name]!.conversationId;
				state.reporters[api.taskId] = await tx.createTask(Reporter, { name, conversationId, message, followUp: action === "send" && followUp === true }, background);
				return action === "send" ? `Sent to ${name}.` : `Started ${name}.`;
			}, callContext);
			return reply(result, (await api.snapshot(Subagents, api.conversationId, callContext))?.agents[name]?.conversationId);
		},
	});
	// The task definitions come with the extension, so reporters resume after a restart.
	const extension: Extension = defineExtension({ name: "subagent", tasks: [Anchor, Reporter], tools: [tool] });
	return extension;
}
