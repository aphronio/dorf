import { BACKGROUND_CONTEXT, type Context } from "@earendil-works/chord/context";
import type { AssistantMessage } from "@earendil-works/pi-ai";
import {
	AssistantEntry,
	type Conversation,
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
// Each message to a subagent is work in the registry until its answer is taken: by the call that sent it; once the
// owner's message ends that call's wait, by a reporter that sends it to the Thread or by a later call without a
// message; after a stop, by `placeStopped`. `claims` names the call or reporter each answer went to, so each answer
// reaches the Thread once.
type Work = { requestId: string; message: string; waiter?: TaskId; reporter?: TaskId };
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
	const head = Buffer.from(text).subarray(0, CAP - 256).toString().replace(/�$/, "");
	return `${head}\n\n[Answer truncated: ${bytes - Buffer.byteLength(head)} bytes omitted. The whole answer is in ${name}'s transcript.]`;
}

const own = <T>(record: Record<string, T>, key: string) => (Object.hasOwn(record, key) ? record[key] : undefined);
const live = async (tx: Tx, id: TaskId | undefined) => {
	const task = id === undefined ? undefined : await tx.task(id);
	return task !== undefined && task.state.status !== "terminal" && !task.abortRequested;
};

// Takes a settled answer for a call or reporter, or for none after a stop: its text, what became of it, or the failure.
type Taken = { text: string; isError?: true; entry?: EntryId };
const REPORTER = "dorf.subagent-reporter";
async function take(tx: Tx, agent: Subagent, name: string, settled: SettledSubmissionRecord, by: TaskId | undefined): Promise<Taken> {
	if (settled.status === "unanswered") return { text: `${name} ${settled.reason === "aborted" ? "was stopped" : `failed: ${settled.reason}`}.`, isError: true };
	if (settled.type !== "input") return { text: `${name} gave no answer.`, isError: true };
	const entry = settled.answer;
	const taker = own(agent.claims ?? {}, String(entry));
	const arrived = `${name}'s answer to this message already arrived as a message.`;
	if (taker !== undefined && taker !== by)
		return { text: (await tx.task(taker))?.kind === REPORTER ? arrived : `${name} answered this message together with another one, in the answer another call returned.` };
	if (taker === undefined && agent.reported.includes(entry)) return { text: arrived };
	const message = (await tx.entry(AssistantEntry, entry))?.model?.[0] as AssistantMessage | undefined;
	return { text: cap((message?.content ?? []).flatMap((c) => (c.type === "text" ? [c.text] : [])).join(""), name), entry };
}
// Written after any admission in the same commit, which must come before every write.
function keep(agent: Subagent, taken: Taken, by: TaskId | undefined, requestId: string): void {
	if (taken.entry !== undefined && !agent.reported.includes(taken.entry)) agent.reported.push(taken.entry);
	// Assigned, then written through the document's own copy: a write to the object assigned would be lost.
	if (taken.entry !== undefined && by !== undefined) {
		agent.claims ??= {};
		agent.claims[String(taken.entry)] = by;
	}
	agent.work = agent.work?.filter((w) => w.requestId !== requestId);
}

// Sends the Thread the answer to work the owner's message handed off, unless a call takes it. The request ID finds the
// submission the call made. A reporter from before waiting calls has no `requestId` and may be in `report`.
type ReporterInput = { name: string; conversationId: ConversationId; message: string; requestId?: string };
type ReporterState = { phase: "deliver" } | { phase: "report"; report?: string; failed?: true };
const reportEntry = (name: string, failed?: true) => ({ kind: SubagentReport.kind, data: { name, ...(failed ? { failed } : {}) } });
const Reporter = defineTask<ReporterInput, ReporterState, null>({
	name: REPORTER,
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
				const work = agent.work?.find((w) => w.requestId === requestId);
				// Taken already, or a call waits for it and takes it: the call's own commit decides, and a call whose wait the
				// owner's message ends again starts another reporter. A stop ends both, and `placeStopped` takes it.
				if ((reporter.input.requestId !== undefined && work === undefined) || (await live(tx, work?.waiter))) return done;
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

// After a stop ended every call and reporter: an answer that came before the stop and that none of them took goes into
// the Thread without starting a run; the rest of the stopped work is dropped.
export async function placeStopped(thread: Conversation, context: Context): Promise<void> {
	await thread.commit(async (tx) => {
		const state = await tx.doc(Subagents, thread.id);
		const ended: { agent: Subagent; name: string; requestId: string; taken: Taken }[] = [];
		for (const [name, agent] of Object.entries(state.agents))
			for (const { requestId, waiter, reporter } of agent.work ?? []) {
				if ((await live(tx, waiter)) || (await live(tx, reporter))) continue;
				const record = await tx.submissionByRequest(agent.conversationId, requestId);
				if (record?.status === "done" || record?.status === "unanswered") ended.push({ agent, name, requestId, taken: await take(tx, agent, name, record as SettledSubmissionRecord, undefined) });
			}
		for (const { name, requestId, taken } of ended) {
			if (taken.entry === undefined) continue;
			const model = [{ role: "user" as const, content: `[subagent ${name} answered] ${taken.text}`, timestamp: Date.now() }];
			await thread.submit(tx, { type: "write", entry: { ...reportEntry(name), model }, requestId: `subagent-stopped:${requestId}` });
		}
		for (const { agent, requestId, taken } of ended) keep(agent, taken, undefined, requestId);
	}, context);
}

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
type Target = { conversationId: ConversationId; requestId: string; message: string };

export function subagents(catalog: Catalog): Extension {
	const tool = defineTool({
		name: "subagent",
		description:
			"Send a message to a subagent and wait for its answer. A new name starts a subagent that works in this Thread's " +
			"machine and remembers its earlier messages; model (another offered model's id or name, e.g. a stronger one for " +
			"a hard question) applies only then. A message to a busy subagent steers its current work. Without a message, " +
			"waits for the answer it is working on. Calls in one answer run at once.",
		parameters: obj({ name: str(), message: opt(str()), model: opt(str()) }),
		// A rerun after a restart finds its target in its memo and the same submission by its request ID.
		replay: "safe",
		execute: async ({ name, message, model }, api, callContext) => {
			let target = await api.memo<Target>("target", callContext);
			if (target === undefined) {
				const known = own((await api.snapshot(Subagents, api.conversationId, callContext))?.agents ?? {}, name);
				if (known === undefined && message === undefined) return result({ text: `No subagent named ${name}. Send it a message to start it.`, isError: true });
				// A subagent starts as a copy of the Thread's agent; a model asked for runs at the level nearest the Thread's.
				const choice = known === undefined && model !== undefined ? catalog.near(model, (await api.agent(callContext)).thinkingLevel) : {};
				const found = await api.commit(async (tx): Promise<Target | undefined> => {
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
					agent.work ??= [];
					const work = agent.work;
					if (message !== undefined) {
						const requestId = `subagent:${api.taskId}`;
						if (!work.some((w) => w.requestId === requestId)) work.push({ requestId, message, waiter: api.taskId });
						return { conversationId, requestId, message };
					}
					// Without a message: work this call took before a restart, else the newest work no call waits for.
					let next = work.find((w) => w.waiter === api.taskId);
					for (let i = work.length - 1; next === undefined && i >= 0; i--) if (!(await live(tx, work[i]!.waiter))) next = work[i];
					if (next === undefined) return undefined;
					next.waiter = api.taskId;
					return { conversationId, requestId: next.requestId, message: next.message };
				}, callContext);
				if (found === undefined) return result({ text: `${name} has no work waiting for an answer.` });
				// Kept, so a rerun waits for the same answer even after this call's last commit took it.
				target = await api.memo("target", found, callContext);
			}
			const { conversationId, requestId } = target;

			const subagent = (await api.conversation(conversationId, callContext))!;
			const submission = await subagent.submit({ type: "input", content: target.message, whenBusy: "steer", requestId }, callContext);
			const woke = await Promise.race([submission.wait(callContext), ownerWrites(api, callContext)]);
			const outcome = await api.commit(async (tx) => {
				// An answer that came with the owner's message is still this call's: the reporter defers to a waiting call.
				const record = woke ?? (await tx.submissionByRequest(conversationId, requestId));
				const settled = record?.status === "done" || record?.status === "unanswered" ? (record as SettledSubmissionRecord) : undefined;
				const agent = (await tx.doc(Subagents, api.conversationId)).agents[name]!;
				if (settled === undefined) {
					// Handed off to a reporter, which a later call without a message can take the work from.
					const work = agent.work?.find((w) => w.requestId === requestId);
					const reporter = (await live(tx, work?.reporter))
						? work!.reporter
						: await tx.createTask(Reporter, { name, conversationId, message: target.message, requestId }, { ownership: { kind: "conversation" }, background: true });
					agent.work = [...(agent.work ?? []).filter((w) => w !== work), { requestId, message: target.message, reporter }];
					return result({ text: `Stopped waiting because the owner sent a message. ${name} is still working; its answer will arrive as a message.` });
				}
				const taken = await take(tx, agent, name, settled, api.taskId);
				keep(agent, taken, api.taskId, requestId);
				return result(taken);
			}, callContext);
			return outcome;
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
