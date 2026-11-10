import { call, createThread, fail, killAgent, log, runEnded, sandboxes, send, startAgent, until } from "./kit.ts";

// Long enough that no Sandbox pauses on its own during the proof.
await startAgent("faux", 60 * 60_000);

const start = (i: number) => `$ echo ${i} > /workspace/mark && (nohup sleep 100000 >/dev/null 2>&1 & echo $! > /workspace/pid)`;
const threads: { id: number; sandbox: string }[] = [];
const states = () => Promise.all(threads.map((t) => sandboxes.state(t.sandbox)));

let paused: string[] = [];
for (let i = 1; !paused.length; i++) {
	if (i > 10) await fail("ten Sandboxes ran at once; the worker never filled");
	const thread = await createThread(start(i));
	threads.push(thread);
	await runEnded(thread.id, start(i));
	paused = (await states()).flatMap((s, j) => (s === "paused" ? [threads[j].sandbox] : []));
}
const slots = threads.length - 1;
if (paused.length !== 1 || paused[0] !== threads[0].sandbox) await fail(`expected only the first Sandbox paused, got ${paused}`);
log(`1. the worker held ${slots} Sandboxes; Thread ${slots + 1} paused the one idle longest`);

const check = "$ cat /workspace/mark && kill -0 $(cat /workspace/pid) && echo alive";
await send(threads[0].id, check);
const [back] = await runEnded(threads[0].id, check);
if (back.error || back.output.trim() !== "1\nalive") await fail(`the first Thread came back as ${JSON.stringify(back)}`);
const now = await states();
if (now[1] !== "paused" || now.filter((s) => s === "running").length !== slots) await fail(`after its return: ${now}`);
log("2. the first Thread resumed with its file and process; the next idle one made room", now.join(" "));

const busy = threads.filter((_, j) => now[j] === "running");
for (const t of busy) await send(t.id, "$ sleep 15");
await until("every awake Thread to run", async () => {
	for (const t of busy) if (!(await call("GET", `/threads/${t.id}`)).json.docs["pi.live"]?.run) return false;
	return true;
});
const waiter = threads[now.indexOf("paused")];
const began = Date.now();
await send(waiter.id, "$ echo waited");
const [waited] = await runEnded(waiter.id, "$ echo waited", 120_000);
const seconds = (Date.now() - began) / 1000;
if (waited.error || waited.output.trim() !== "waited" || seconds < 10) await fail(`with every slot busy: ${JSON.stringify(waited)} after ${seconds}s`);
log("3. with every slot busy the next Thread waited for a run to end, then took its slot", `${seconds.toFixed(1)} s`);

for (const t of threads) if ((await call("DELETE", `/threads/${t.id}`)).status !== 204) await fail("delete");
log("4. threads deleted");

killAgent();
console.log("PASS");
