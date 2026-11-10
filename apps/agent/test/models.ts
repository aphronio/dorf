// Model choice, thinking levels and subagents, on the faux provider's two models.
import { SandboxNotFound } from "../src/sandbox.ts";
import { call, createThread, fail, IDLE_MS, killAgent, log, runEnded, sandboxes, send, startAgent, until } from "./kit.ts";

type Choice = { ref: string; name: string; thinking: string[] };
type Entry = { kind: string; model?: { role: string; model?: string; isError?: boolean; content?: string | { type: string; text?: string }[] }[] };

await startAgent();

const { json: models } = (await call("GET", "/models")) as { json: Choice[] };
if (models.map((m) => m.ref).join() !== "faux/faux,faux/faux-2") await fail(`models: ${JSON.stringify(models)}`);
if (!models[0].thinking.includes("high") || models[0].thinking.includes("max")) await fail(`thinking levels: ${models[0].thinking}`);
log("1. the default model leads the list, with only the levels it offers", models[0].thinking.join(" "));

for (const [body, what] of [
	[{ model: "faux/nope" }, "an unknown model"],
	[{ model: "faux/faux-2", thinking: "max" }, "a level the model lacks"],
] as const) {
	const refused = await call("POST", "/threads", { prompt: "hi", ...body });
	if (refused.status !== 400) await fail(`${what}: ${refused.status}`);
}
const first = "$ echo hello";
const { id, sandbox } = await createThread(first, undefined, { model: "faux/faux-2", thinking: "low" });
await runEnded(id, first);
const view = (await call("GET", `/threads/${id}`)).json;
const replied = (view.entries as Entry[]).filter((e) => e.kind === "pi.assistant").map((e) => e.model?.[0]?.model);
if (view.agent.model.modelId !== "faux-2" || view.agent.thinkingLevel !== "low" || !replied.every((m) => m === "faux-2"))
	await fail(`thread on faux-2: ${JSON.stringify(view.agent)} replied by ${replied}`);
log("2. a Thread runs on the model and level it was created with; unknown models and levels are refused");

if ((await call("POST", `/threads/${id}/thinking`, { level: "medium" })).status !== 202) await fail("thinking change");
if ((await call("POST", `/threads/${id}/thinking`, { level: "max" })).status !== 400) await fail("max was accepted");
const changed = (await call("GET", `/threads/${id}`)).json.agent.thinkingLevel;
if (changed !== "medium") await fail(`thinking after change: ${changed}`);
log("3. thinking changes mid-Thread; a level the model lacks is refused");

// Reports from subagents arrive as user messages, so an answer is found by its message, not as the newest one.
const text = (e: Entry) => (e.model ?? []).flatMap((m) => (typeof m.content === "string" ? [m.content] : (m.content ?? []).map((c) => c.text ?? ""))).join("");
async function answered(message: string): Promise<{ output: string; error: boolean }[]> {
	return until(`the answer to ${JSON.stringify(message.slice(0, 50))}`, async () => {
		const entries: Entry[] = (await call("GET", `/threads/${id}`)).json.entries;
		const asked = entries.findLastIndex((e) => e.kind === "pi.user" && text(e) === message);
		if (asked < 0) return undefined;
		const next = entries.findIndex((e, i) => i > asked && e.kind === "pi.user");
		const turn = entries.slice(asked + 1, next < 0 ? undefined : next);
		if (!turn.some((e) => e.kind === "pi.assistant" && text(e) === "done")) return undefined;
		return turn.filter((e) => e.kind === "pi.tool-result").map((e) => ({ output: text(e), error: Boolean(e.model?.[0]?.isError) }));
	});
}
async function ask(message: string) {
	await send(id, message);
	return answered(message);
}
const reports = async (name: string) =>
	((await call("GET", `/threads/${id}`)).json.entries as (Entry & { data?: { name: string; failed?: true } })[]).filter(
		(e) => e.kind === "dorf.subagent-report" && e.data?.name === name && !e.data.failed,
	).length;
const subagent = (args: object) => `@ ${JSON.stringify(args)}`;

const [started] = await ask(subagent({ action: "spawn", name: "helper", message: "$ echo from-child > child.txt" }));
if (started?.output !== "Started helper.") await fail(`spawn: ${JSON.stringify(started)}`);
await until("helper's report", async () => (await reports("helper")) === 1);
await ask(subagent({ action: "send", name: "helper", message: "$ echo again >> child.txt" }));
await until("helper's second report", async () => (await reports("helper")) === 2);
const [again] = await ask(subagent({ action: "spawn", name: "helper", message: "x" }));
const [file] = await ask("$ cat child.txt");
const [status] = await ask(subagent({ action: "status" }));
if (!again?.output.includes("already exists") || file?.output.trim() !== "from-child\nagain" || status?.output !== "helper: idle")
	await fail(`persistent subagent: ${JSON.stringify({ again, file, status })}`);
log("4. a subagent works in the Thread's machine, keeps its conversation across messages, and reports each answer");

const [other] = await ask(subagent({ action: "spawn", name: "second", model: "faux/faux-2", message: "$ true" }));
const [unknown] = await ask(subagent({ action: "spawn", name: "third", model: "faux/nope", message: "$ true" }));
if (other?.output !== "Started second." || !unknown?.error) await fail(`subagent models: ${JSON.stringify({ other, unknown })}`);
log("5. a subagent can run on another offered model; an unknown one fails the call");

await ask(subagent({ action: "spawn", name: "sleeper", message: "$[60] sleep 16" }));
await new Promise((r) => setTimeout(r, IDLE_MS + 3_000));
if ((await sandboxes.state(sandbox)) !== "running") await fail("the Sandbox paused under a working subagent");
await until("sleeper's report", async () => (await reports("sleeper")) === 1, 30_000);
await until("the idle pause after the report", async () => (await sandboxes.state(sandbox)) === "paused", IDLE_MS + 30_000);
log("6. a working subagent keeps the Sandbox awake; the Thread pauses once its report is in");

await ask(subagent({ action: "spawn", name: "long", message: "$[120] sleep 120" }));
await until("long to wake the paused Sandbox", async () => (await sandboxes.state(sandbox)) === "running", 30_000);
await new Promise((r) => setTimeout(r, IDLE_MS + 3_000));
const [stopped] = await ask(subagent({ action: "stop", name: "long" }));
await until("long to stop", async () => (await ask(subagent({ action: "status", name: "long" })))[0]?.output === "long: idle", 30_000);
if (stopped?.output !== "Stopped long.") await fail(`stop: ${JSON.stringify(stopped)}`);
await until("the idle pause after a stop, which sends no report", async () => (await sandboxes.state(sandbox)) === "paused", 2 * IDLE_MS + 30_000);
log("7. a subagent wakes the paused Sandbox; stop ends its work, leaves it usable, and the Thread still pauses");

await ask(subagent({ action: "spawn", name: "worker", message: "$[60] sleep 6 && echo ran >> runs" }));
await new Promise((r) => setTimeout(r, 2_000));
killAgent("SIGKILL");
await startAgent();
await until("worker's report after the restart", async () => (await reports("worker")) === 1, 60_000);
const [runs] = await ask("$ wc -l < runs");
if (runs?.output.trim() !== "1") await fail(`the subagent's command ran ${runs?.output.trim()} times`);
log("8. an agent killed while a subagent works resumes it; the command ran once and the report arrived");

await ask(subagent({ action: "spawn", name: "busy", message: "$[120] sleep 120" }));
const deleting = Date.now();
if ((await call("DELETE", `/threads/${id}`)).status !== 204) await fail("delete");
if (!(await sandboxes.state(sandbox).then(() => false, (e) => e instanceof SandboxNotFound))) await fail("the Sandbox outlived its Thread");
log("9. deleting a Thread stops its subagents and removes the Sandbox", `${((Date.now() - deleting) / 1000).toFixed(1)}s`);
console.log("PASS");
process.exit(0);
