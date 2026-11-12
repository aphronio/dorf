// Model choice, thinking levels and subagents, on the faux provider's two models. subagent-restarts.ts proves
// what happens to subagents when the agent is killed or the Thread is stopped.
import { call, createThread, fail, IDLE_MS, log, pass, runEnded, sandboxes, send, startAgent, until } from "./kit.ts";
import { type Entry, STOPPED_WAITING, subagent, subagentThread, text } from "./subagents.ts";

type Choice = { ref: string; name: string; thinking: string[] };

await startAgent();

const { json: models } = (await call("GET", "/models")) as { json: Choice[] };
if (models.map((m) => m.ref).join() !== "faux/faux,faux/faux-2") await fail(`models: ${JSON.stringify(models)}`);
if (!models[0].thinking.includes("high") || models[0].thinking.includes("max")) await fail(`thinking levels: ${models[0].thinking}`);
log("1. the default model leads the list, then the newest of each family, with only the levels it offers", models[0].thinking.join(" "));

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
await send(id, "?");
await until("the system prompt", async () =>
	((await call("GET", `/threads/${id}`)).json.entries as Entry[]).some((e) => e.kind === "pi.assistant" && text(e).includes("Models you can give a subagent: faux (faux), faux-2 (Faux 2)")),
);
log("2. a Thread runs on the model and level it was created with and is told the models; unknown models and levels are refused");

if ((await call("POST", `/threads/${id}/thinking`, { level: "medium" })).status !== 202) await fail("thinking change");
if ((await call("POST", `/threads/${id}/thinking`, { level: "max" })).status !== 400) await fail("max was accepted");
const changed = (await call("GET", `/threads/${id}`)).json.agent.thinkingLevel;
if (changed !== "medium") await fail(`thinking after change: ${changed}`);
log("3. thinking changes mid-Thread; a level the model lacks is refused");

const write = "$ printf 'Run the proofs before you push.\\n' > AGENTS.md && mkdir -p .agents/skills/browser && printf -- '---\\nname: browser\\ndescription: >-\\n  Drive a browser: <CDP>\\n  over WebSocket.\\n---\\n' > .agents/skills/browser/SKILL.md";
await send(id, write);
await runEnded(id, write);
await send(id, "?");
await until("the project's instructions and skills", async () =>
	((await call("GET", `/threads/${id}`)).json.entries as Entry[]).some((e) => {
		const told = e.kind === "pi.assistant" ? text(e) : "";
		return told.includes("/AGENTS.md\">\nRun the proofs before you push.") && told.includes("<description>Drive a browser: &lt;CDP&gt; over WebSocket.</description>") && told.includes("/.agents/skills/browser/SKILL.md</location>");
	}),
);
log("4. AGENTS.md and the skills in the Thread's machine reach its next request");

const { answered, ask, entries, events, reports, results, working } = await subagentThread(id);

const [wrote] = await ask(subagent({ name: "helper", message: "$ echo from-child > child.txt\n= wrote child.txt" }));
await until("helper in the streamed subagents", async () => events().some((e) => e.subagents?.some((s: { name: string }) => s.name === "helper")), 10_000);
const [recalled] = await ask(subagent({ name: "helper", message: "^" }));
const [file] = await ask("$ cat child.txt");
if (wrote?.output !== "wrote child.txt" || recalled?.output !== "before: $ echo from-child > child.txt\n= wrote child.txt" || file?.output.trim() !== "from-child" || (await reports("helper")).length)
	await fail(`persistent subagent: ${JSON.stringify({ wrote, recalled, file, reports: await reports("helper") })}`);
log("5. a call returns the subagent's answer; a second message to the same name gets an answer that uses the first; it works in the Thread's machine and the stream lists it");

const pair = Date.now();
const both = await ask(`@ ${JSON.stringify([{ name: "a", message: "$ sleep 4\n= alpha" }, { name: "b", message: "$ sleep 4\n= beta" }])}`);
const took = (Date.now() - pair) / 1000;
if (both.map((r) => r.output).sort().join() !== "alpha,beta" || took >= 8) await fail(`two calls in one round: ${JSON.stringify(both)} in ${took}s`);
log("6. two calls in one round run at once and each returns its own subagent's answer", `${took.toFixed(1)}s for two 4 s jobs`);

const [other] = await ask(subagent({ name: "second", model: "other/claude-Faux-2", message: "$ true" }));
const [unknown] = await ask(subagent({ name: "third", model: "anthropic/nope", message: "$ true" }));
if (other?.output !== "done" || !unknown?.error || !unknown.output.includes("faux-2")) await fail(`subagent models: ${JSON.stringify({ other, unknown })}`);
log("7. a subagent runs on an offered model however a model names it; an unknown one fails with the list");

const nap = `$[60] sleep ${IDLE_MS / 1000 + 6}\n= slept`;
await send(id, subagent({ name: "slow", message: nap }));
await until("slow to work", async () => working("slow"), 10_000);
const owner = "$ echo owner";
const interrupted = Date.now();
await send(id, owner);
const [echoed] = await answered(owner);
const reached = (Date.now() - interrupted) / 1000;
if (echoed?.output.trim() !== "owner" || !working("slow") || !(await results()).some((r) => r.output === STOPPED_WAITING("slow")))
	await fail(`the owner's message mid-wait: ${JSON.stringify({ echoed, working: working("slow"), results: await results() })}`);
await new Promise((r) => setTimeout(r, IDLE_MS + 1_000));
if ((await sandboxes.state(sandbox)) !== "running") await fail("the Sandbox paused under a working subagent");
const [slept] = await until("slow's report", async () => (await reports("slow")).length > 0 && (await reports("slow")), 30_000);
await until("the Thread to answer the report", async () => (await entries()).at(-1)?.kind === "pi.assistant" && text((await entries()).at(-1)!) === "done");
await new Promise((r) => setTimeout(r, 2_000));
if (slept !== "[subagent slow answered] slept" || (await reports("slow")).length !== 1 || (await results()).some((r) => r.output.includes("slept")))
	await fail(`slow's answer: ${JSON.stringify({ reports: await reports("slow"), results: await results() })}`);
await until("the idle pause after the report", async () => (await sandboxes.state(sandbox)) === "paused", IDLE_MS + 30_000);
log("8. the owner's message ends the wait and reaches the Thread in the next round; the subagent keeps the Sandbox awake and its answer arrives once, as a message", `${reached.toFixed(1)}s`);

const [idle] = await ask(subagent({ name: "slow" }));
await send(id, subagent({ name: "slow", message: "$[60] sleep 5\n= second nap" }));
await until("slow to work again", async () => working("slow"), 30_000);
await ask("$ true");
const [takenOver] = await ask(subagent({ name: "slow" }));
await new Promise((r) => setTimeout(r, 2_000));
if (idle?.output !== "slow has no work waiting for an answer." || takenOver?.output !== "second nap" || (await reports("slow")).length !== 1)
	await fail(`a call without a message: ${JSON.stringify({ idle, takenOver, reports: await reports("slow") })}`);
log("9. a call without a message takes over work the owner's message handed off and returns its answer, which then arrives no other way");

await until("the idle pause", async () => (await sandboxes.state(sandbox)) === "paused", 2 * IDLE_MS + 30_000);
const long = subagent({ name: "long", message: "$[120] sleep 120" });
await send(id, long);
await until("long to wake the paused Sandbox", async () => (await sandboxes.state(sandbox)) === "running", 30_000);
await until("long to work", async () => working("long"), 30_000);
if ((await call("POST", `/threads/${id}/abort`)).status !== 202) await fail("abort");
await until("long to stop with the Thread", async () => !working("long"), 30_000);
const [back] = await ask(subagent({ name: "long", message: "= back" }));
if (back?.output !== "back" || (await reports("long")).length) await fail(`long after a stop: ${JSON.stringify({ back, reports: await reports("long") })}`);
await until("the idle pause after a stop", async () => (await sandboxes.state(sandbox)) === "paused", 2 * IDLE_MS + 30_000);
log("10. a subagent wakes the paused Sandbox; the stop button stops the subagent the Thread waits for, the same name answers a later call, and the Thread still pauses");
await pass();
