// What happens to a Thread's subagents when the agent is killed or the Thread is stopped, on the faux model.
import { SandboxNotFound } from "../src/sandbox.ts";
import { call, createThread, fail, follow, log, pass, runEnded, sandboxes, send, startAgent, until } from "./kit.ts";
import { sleep, STOPPED_WAITING, subagent, subagentThread, TOGETHER } from "./subagents.ts";

await startAgent();
const first = "$ echo hello";
const { id, sandbox } = await createThread(first);
await runEnded(id, first);
const { answered, ask, handOff, reports, restart, results, working, entries } = await subagentThread(id);

const job = subagent({ name: "worker", message: "$[60] sleep 6 && echo ran >> runs\n= worked" });
await send(id, job);
await until("worker to work", async () => working("worker"), 30_000);
await new Promise((r) => setTimeout(r, 1_000));
await restart();
const [worked] = await answered(job, 60_000);
await new Promise((r) => setTimeout(r, 2_000));
const [runs] = await ask("$ wc -l < runs");
const answers = (await results()).filter((r) => r.output === "worked").length + (await reports("worker")).length;
if (worked?.output !== "worked" || answers !== 1 || runs?.output.trim() !== "1") await fail(`restart mid-wait: ${JSON.stringify({ worked, answers, runs })}`);
log("1. an agent killed while the Thread waits answers the call exactly once; the command ran once");

const twins = await ask(`@ ${JSON.stringify([{ name: "twin", message: "$ sleep 2\n= one" }, { name: "twin", message: "= two" }])}`);
await sleep(2_000);
const own = twins.map((r) => r.output).filter((o) => o !== TOGETHER("twin"));
if (twins.length !== 2 || twins.some((r) => r.error) || !own.length || own.some((o) => o !== "one" && o !== "two") || new Set(own).size !== own.length || (await reports("twin")).length)
	await fail(`two calls to one name: ${JSON.stringify({ twins, reports: await reports("twin") })}`);
log("2. two calls to one name in one round: each returns an answer, and an answer to both reaches the Thread once", twins.map((r) => JSON.stringify(r.output)).join(" and "));

await handOff("bg", "$[120] sleep 120\n= bg done");
if ((await call("POST", `/threads/${id}/abort`)).status !== 202) await fail("abort");
await until("bg to stop with the Thread", async () => !working("bg"), 30_000);
await sleep(3_000);
const lastBeforeBack = (await entries()).at(-1);
const [bgBack] = await ask(subagent({ name: "bg", message: "= bg back" }));
if ((await reports("bg")).length || lastBeforeBack?.kind === "dorf.subagent-report" || bgBack?.output !== "bg back") await fail(`stop after a handoff: ${JSON.stringify({ reports: await reports("bg"), bgBack })}`);
log("3. the stop button also stops handed-off work and its reporter: no answer arrives and no run starts, and the same name answers later");

await handOff("relay", "$[60] sleep 5 && echo ran >> relay-runs\n= relayed");
await restart();
await until("relay's report after the restart", async () => (await reports("relay")).length > 0, 60_000);
await sleep(2_000);
const [relayRuns] = await ask("$ wc -l < relay-runs");
if ((await reports("relay")).join() !== "[subagent relay answered] relayed" || relayRuns?.output.trim() !== "1") await fail(`restart after a handoff: ${JSON.stringify({ reports: await reports("relay"), relayRuns })}`);
log("4. an agent killed after the owner's message handed work off reports its answer once; the command ran once");

await handOff("pair", "$[60] sleep 5\n= first");
await send(id, subagent({ name: "pair", message: "= second" }));
await send(id, "$ true");
await until("the second handoff", async () => (await results()).filter((r) => r.output === STOPPED_WAITING("pair")).length === 2, 30_000);
const [newest] = await ask(subagent({ name: "pair" }));
await sleep(2_000);
if (newest?.output !== "second" || (await reports("pair")).length) await fail(`a takeover after two handoffs: ${JSON.stringify({ newest, reports: await reports("pair") })}`);
log("5. after two handoffs to one name, a call without a message takes the newest; the steered subagent answered both at once, and that answer arrives once");

const busy = follow(`/threads/${id}/events`);
await send(id, subagent({ name: "busy", message: "$[120] sleep 120" }));
await until("busy to work", async () => busy.events.findLast((e) => e.subagents)?.subagents?.some((s: { name: string; working: boolean }) => s.name === "busy" && s.working), 30_000);
busy.close();
const deleting = Date.now();
if ((await call("DELETE", `/threads/${id}`)).status !== 204) await fail("delete");
if (!(await sandboxes.state(sandbox).then(() => false, (e) => e instanceof SandboxNotFound))) await fail("the Sandbox outlived its Thread");
log("6. deleting a Thread stops its subagents and removes the Sandbox", `${((Date.now() - deleting) / 1000).toFixed(1)}s`);
await pass();
