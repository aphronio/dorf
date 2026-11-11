import { BACKGROUND_CONTEXT, type Context } from "@earendil-works/chord/context";
import type { AssistantMessage } from "@earendil-works/pi-ai";
import {
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
	InboxDoc,
	section,
	type SettledSubmissionRecord,
	type TaskId,
	type Tx,
} from "@earendil-works/pi-durable";
import { obj, opt, str } from "@earendil-works/pi-schema";
import type { Catalog } from "./models.ts";
import { ThreadSandbox } from "./thread-sandbox.ts";

// Persistent subagents, after Pi's examples 22 (a registry and an anchor) and 43 (a call that waits for the answer).
// A call sends its message as a steer and waits for the answer. When the owner's message ends the wait, the work is
// handed off: a reporter reports the answer to the Thread as a message, unless a later call takes the work over.
// `claims` names the call or reporter each answer went to, so each answer reaches the Thread once.
type Work = { requestId: string; message: string; waiter?: TaskId };
type Subagent = { conversationId: ConversationId; reported: EntryId[]; claims?: Record<string, TaskId>; work?: Work[] };
// A subagent's answer in the Thread: the model reads its text, the page its author.
export const SubagentReport = defineEntry<{ name: string; failed?: true }>("dorf.subagent-report");
// Created with its Thread, so the page can follow it from the start: a watch never creates a document.
export const Subagents = defineDoc<{ agents: Record<string, Subagent> }>({
	checkpointWhen: () => false,
	kind: "dorf.subagents",
	version: 1,
	scope: "conversation",
	history: "latest",
	fork: "initial",
	initial: () => ({ agents: {} }),
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

// Pi coding agent's subagent cap: the head of the answer, with room for the notice.
const CAP = 50 * 1024;
function cap(text: string, name: string): string {
	const bytes = Buffer.byteLength(text);
	if (bytes <= CAP) return text;
	const head = Buffer.from(text).subarray(0, CAP - 256).toString().replace(/\uFFFD$/, "");
	return `${head}\n\n[Answer truncated: ${bytes - Buffer.byteLength(head)} bytes omitted. The whole answer is in ${name}'s transcript.]`;
}

const own = <T>(record: Record<string, T>, key: string) => (Object.hasOwn(record, key) ? record[key] : undefined);
const live = async (tx: Tx, id: TaskId | undefined) => {
	const task = id === undefined ? undefined : await tx.task(id);
	return task !== undefined && task.state.status !== "terminal" && !task.abortRequested;
};

// Takes a settled submission's answer for `by`: its text once, a note to any other taker, or the failure.
type Taken = { text: string; isError?: true; entry?: EntryId };
async function take(tx: Tx, agent: Subagent, name: string, settled: SettledSubmissionRecord, by: TaskId): Promise<Taken> {
	if (settled.status === "unanswered") return { text: `${name} ${settled.reason === "aborted" ? "was stopped" : `failed: ${settled.reason}`}.`, isError: true };
	if (settled.type !== "input") return { text: `${name} gave no answer.`, isError: true };
	const entry = settled.answer;
	const claims = (agent.claims ??= {});
	const taker = own(claims, String(entry));
	if (taker !== undefined ? taker !== by : agent.reported.includes(entry)) return { text: `${name} answered this message together with another one; that answer went to another call.` };
	const message = (await tx.entry(AssistantEntry, entry))?.model?.[0] as AssistantMessage | undefined;
	return { text: cap((message?.content ?? []).flatMap((c) => (c.type === "text" ? [c.text] : [])).join(""), name), entry };
}
// Written after any admission in the same commit, which must come before every write.
function keep(agent: Subagent, taken: Taken, by: TaskId, requestId: string): void {
	if (taken.entry !== undefined && !agent.reported.includes(taken.entry)) agent.reported.push(taken.entry);
	if (taken.entry !== undefined) agent.claims![String(taken.entry)] = by;
	agent.work = agent.work?.filter((w) => w.requestId !== requestId);
}

// Reports the answer to work the owner's message handed off, unless a call took the work over. The request ID finds
// the submission the call made. A reporter from before waiting calls has no `requestId` and may be in `report`.
type ReporterInput = { name: string; conversationId: ConversationId; message: string; requestId?: string };
type ReporterState = { phase: "deliver" } | { phase: "report"; report?: string; failed?: true };
const reportEntry = (name: string, failed?: true) => ({ kind: SubagentReport.kind, data: { name, ...(failed ? { failed } : {}) } });
const Reporter = defineTask<ReporterInput, ReporterState, null>({
	name: "dorf.subagent-reporter",
	version: 1,
	initial: () => ({ phase: "deliver" }),
	phases: {
		deliver: async (reporter, runtime, taskContext) => {
			const { name, conversationId, message } = reporter.input;
			const requestId = reporter.input.requestId ?? `subagent:${reporter.id}`;
			const subagent = (await runtime.conversation(conversationId, taskContext))!;
			const settled = await (await subagent.submit({ type: "input", content: message, whenBusy: "steer", requestId }, taskContext)).wait(taskContext);
			const thread = (await runtime.conversation(runtime.conversationId, taskContext))!;
			await runtime.commit(async (tx) => {
				const done = { status: "terminal", outcome: { status: "completed", result: null } } as const;
				const agent = (await tx.doc(Subagents, runtime.conversationId)).agents[name]!;
				if (await live(tx, agent.work?.find((w) => w.requestId === requestId)?.waiter)) return done;
				const taken = await take(tx, agent, name, settled, reporter.id);
				const stopped = settled.status === "unanswered" && settled.reason === "aborted";
				const shared = taken.entry === undefined && !taken.isError;
				if (!stopped && !shared) {
					const content = taken.isError ? `[subagent ${taken.text}]` : `[subagent ${name} answered] ${taken.text}`;
					await thread.submit(tx, { type: "input", content, entry: reportEntry(name, taken.isError), whenBusy: "steer", requestId: `subagent-report:${reporter.id}` });
				}
				keep(agent, taken, reporter.id, requestId);
				return done;
			}, taskContext);
		},
		report: async (reporter, runtime, taskContext) => {
			const { report, failed } = reporter.state.checkpoint;
			if (report !== undefined) {
				const thread = (await runtime.conversation(runtime.conversationId, taskContext))!;
				await thread.submit({ type: "input", content: report, entry: reportEntry(reporter.input.name, failed), whenBusy: "steer", requestId: `subagent-report:${reporter.id}` }, taskContext);
			}
			await runtime.commit(() => ({ status: "terminal", outcome: { status: "completed", result: null } }), taskContext);
		},
	},
	abort: (_reporter, runtime, taskContext) => runtime.commit(() => ({ status: "terminal", outcome: { status: "aborted" } }), taskContext),
});

// Resolves once the owner's own message waits in the Thread's inbox: a subagent's report is an entry of its own kind.
async function ownerWrites(api: { conversationId: ConversationId; watchDoc: Harness["watchDoc"] }, context: Context): Promise<void> {
	const inbox = (await api.watchDoc(InboxDoc, api.conversationId, context))!;
	const wrote = () => inbox.value.items.some((item) => item.mode !== "write" && item.entry === undefined);
	try {
		await new Promise<void>((resolve, reject) => {
			context.abortSignal?.addEventListener("abort", () => reject(context.abortSignal!.reason ?? new Error("aborted")), { once: true });
			inbox.start(async () => void (wrote() && resolve()));
			if (wrote()) resolve();
		});
	} finally {
		await inbox.stop();
	}
}

const result = ({ text, isError }: { text: string; isError?: boolean }) => ({ output: [{ type: "text" as const, text }], ...(isError ? { isError } : {}) });

export function subagents(catalog: Catalog, harness: () => Harness): Extension {
	const tool = defineTool({
		name: "subagent",
		description:
			"Send a message to a subagent and wait for its answer. A new name starts a subagent that works in this Thread's " +
			"machine and remembers its earlier messages; model (another offered model's id or name, e.g. a stronger one for " +
			"a hard question) applies only then. A message to a busy subagent steers its current work. Without a message, " +
			"waits for the answer it is working on. Calls in one answer run at once.",
		parameters: obj({ name: str(), message: opt(str()), model: opt(str()) }),
		// A rerun after a restart submits under the same request ID, so it finds the same submission and its answer.
		replay: "safe",
		execute: async ({ name, message, model }, api, callContext) => {
			const known = own((await api.snapshot(Subagents, api.conversationId, callContext))?.agents ?? {}, name);
			if (known === undefined && message === undefined) return result({ text: `No subagent named ${name}. Send it a message to start it.`, isError: true });
			// A subagent starts as a copy of the Thread's agent; a model asked for runs at the level nearest the Thread's.
			const choice = known === undefined && model !== undefined ? catalog.near(model, (await api.agent(callContext)).thinkingLevel) : {};

			const target = await api.commit(async (tx) => {
				const state = await tx.doc(Subagents, api.conversationId);
				let agent = own(state.agents, name);
				if (agent === undefined) {
					const anchor = await tx.createTask(Anchor, null, { ownership: { kind: "conversation" }, background: true });
					const child = await tx.createConversation({ ownership: { kind: "task", taskId: anchor } });
					Object.assign(await tx.doc(ThreadSandbox, child.id), await tx.doc(ThreadSandbox, api.conversationId));
					await configure(tx, child.id, {
						extensions: { remove: [extension] },
						instructions: `You are the subagent "${name}". Answer the main agent's requests.`,
						...choice,
					});
					agent = state.agents[name] = { conversationId: child.id, reported: [] };
				}
				const { conversationId } = agent;
				if (message !== undefined) return { conversationId, requestId: `subagent:${api.taskId}`, message };
				// Without a message: handed-off work this call took before a restart, else the newest no call waits for.
				let work = agent.work?.find((w) => w.waiter === api.taskId);
				for (const w of agent.work ?? []) if (work === undefined && !(await live(tx, w.waiter))) work = w;
				if (work === undefined) return undefined;
				work.waiter = api.taskId;
				return { conversationId, requestId: work.requestId, message: work.message };
			}, callContext);
			if (target === undefined) return result({ text: `${name} has no work waiting for an answer.` });
			const { conversationId, requestId } = target;

			// The stop button aborts this call and so the subagent's work; a Harness that closes leaves both to resume.
			callContext.abortSignal?.addEventListener(
				"abort",
				() =>
					void harness()
						.getTask(api.taskId, BACKGROUND_CONTEXT)
						.then(async (task) => {
							if (task?.abortRequested && task.abortReason === undefined) await (await harness().conversation(conversationId, BACKGROUND_CONTEXT))?.abort(BACKGROUND_CONTEXT);
						})
						.catch(() => {}),
				{ once: true },
			);

			const subagent = (await api.conversation(conversationId, callContext))!;
			const submission = await subagent.submit({ type: "input", content: target.message, whenBusy: "steer", requestId }, callContext);
			const woke = await Promise.race([submission.wait(callContext), ownerWrites(api, callContext)]);
			return api.commit(async (tx) => {
				// An answer that came with the owner's message is still this call's: the reporter defers to a waiting call.
				const record = woke ?? (await tx.submissionByRequest(conversationId, requestId));
				const settled = record?.status === "done" || record?.status === "unanswered" ? (record as SettledSubmissionRecord) : undefined;
				const agent = (await tx.doc(Subagents, api.conversationId)).agents[name]!;
				const work = agent.work?.find((w) => w.requestId === requestId);
				if (settled === undefined) {
					// Handed off: the reporter, or a later call that takes the work over, gets the answer.
					if (work !== undefined) delete work.waiter;
					else {
						(agent.work ??= []).push({ requestId, message: target.message });
						await tx.createTask(Reporter, { name, conversationId, message: target.message, requestId }, { ownership: { kind: "conversation" }, background: true });
					}
					return result({ text: `Stopped waiting because the owner sent a message. ${name} is still working; its answer will arrive as a message.` });
				}
				const taken = await take(tx, agent, name, settled, api.taskId);
				keep(agent, taken, api.taskId, requestId);
				return result(taken);
			}, callContext);
		},
	});
	// The task definitions come with the extension, so reporters resume after a restart.
	const extension: Extension = defineExtension({
		name: "subagent",
		tasks: [Anchor, Reporter],
		tools: [tool],
		sections: [section("subagent_models", () => `Models you can give a subagent: ${catalog.offered()}.`)],
	});
	return extension;
}
