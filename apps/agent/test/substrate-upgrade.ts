import { execFileSync, spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { createThread, fail, killAgent, log, needSubstrate, type Result, runEnded, sandboxes, send, startAgent, until } from "./kit.ts";

await needSubstrate();

const SUBSTRATE_SH = new URL("../../../deploy/substrate.sh", import.meta.url).pathname;
const PINNED = readFileSync(new URL("../../../deploy/substrate.env", import.meta.url), "utf8").match(/^SUBSTRATE_VERSION=(\S+)/m)![1];
const ROLL_TIMEOUT_MS = 30 * 60_000;

const json = (args: string[]) => JSON.parse(execFileSync("kubectl", [...args, "-o", "json"], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }));
const installed = () => json(["-n", "ate-system", "get", "deploy", "ate-api-server"]).spec.template.spec.containers[0].image.match(/:([^@]+)/)[1];
const workerOf = (sandbox: string): string | undefined => json(["ate", "get", "actor", sandbox, "-a", "dorf"]).status.workerAssignment?.workerPod;

type Thread = { id: number; sandbox: string };
const setup = "$ echo kept > note.txt && (sleep 100000 & echo $! > pid) && echo started";
const check = "$ cat note.txt && kill -0 $(cat pid) && echo alive";

async function expectIntact(thread: Thread, label: string): Promise<void> {
	await send(thread.id, check);
	const [result] = await runEnded(thread.id, check, 2 * 60_000);
	if (!/kept[\s\S]*alive/.test(result?.output ?? "")) await fail(`${label}: ${JSON.stringify(result)}`);
}

async function paused(thread: Thread): Promise<void> {
	await until(`${thread.sandbox} to pause`, async () => (await sandboxes.state(thread.sandbox)) === "paused", 2 * 60_000);
}

async function keepBusy(thread: Thread, message: string, rolling: () => boolean): Promise<Result[]> {
	const results: Result[] = [];
	while (rolling()) {
		await send(thread.id, message);
		results.push(...(await runEnded(thread.id, message, ROLL_TIMEOUT_MS)));
	}
	return results;
}

async function roll(direction: "upgrade" | "rollback", a: Thread, b: Thread, c: Thread, expected: string): Promise<void> {
	const before = { a: workerOf(a.sandbox), b: workerOf(b.sandbox) };
	let rolling = true;
	const script = spawn("bash", [SUBSTRATE_SH, direction], { stdio: ["ignore", "inherit", "inherit"] });
	const exited = new Promise<number | null>((resolve) => script.on("exit", resolve));
	const busyA = keepBusy(a, Array(5).fill("$ sleep 2 && echo a").join("\n"), () => rolling);
	const busyB = keepBusy(b, "$ sleep 60 && echo b", () => rolling);

	const crashed = new Set<string>();
	let cWoke = false;
	const watch = setInterval(async () => {
		for (const t of [a, b, c]) {
			const state = await sandboxes.state(t.sandbox).catch(() => "unknown");
			if (state === "crashed") crashed.add(t.sandbox);
			if (t === c && state !== "paused") cWoke = true;
		}
	}, 2_000);

	const code = await exited;
	const after = { a: workerOf(a.sandbox), b: workerOf(b.sandbox) };
	rolling = false;
	const results = (await Promise.all([busyA, busyB])).flat();
	clearInterval(watch);

	if (code !== 0) await fail(`${direction} exited ${code}`);
	if (installed() !== expected) await fail(`${direction}: installed ${installed()}, expected ${expected}`);
	if (crashed.size) await fail(`${direction}: crashed ${[...crashed].join(", ")}`);
	if (cWoke) await fail(`${direction}: the paused Thread woke`);
	const failed = results.filter((r) => r.error || !/^[ab]\n$/.test(r.output));
	if (failed.length) await fail(`${direction}: ${failed.length} of ${results.length} tool calls failed or gave wrong output: ${JSON.stringify(failed.slice(0, 3))}`);
	if (after.a === before.a || after.b === before.b) await fail(`${direction}: a busy Thread did not move workers ${JSON.stringify({ before, after })}`);
	log(`${direction} to ${expected}`, `${results.length} tool calls across the roll, none failed; busy Threads moved workers; nothing crashed`);

	for (const [t, label] of [[a, "A"], [b, "B"], [c, "C"]] as const) await expectIntact(t, `${direction}: Thread ${label}`);
	log("   files and processes intact in all three Threads");
	await paused(c);
}

const from = installed();
if (from === PINNED) await fail(`the cluster already runs ${PINNED}; install an earlier release first`);
await startAgent();
const [a, b, c] = [await createThread(setup), await createThread(setup), await createThread(setup)];
for (const t of [a, b, c]) {
	const [started] = await runEnded(t.id, setup);
	if (!started?.output.includes("started")) await fail(`setup: ${JSON.stringify(started)}`);
}
await paused(c);
log("three Threads ready on", from);

await roll("upgrade", a, b, c, PINNED);
await roll("rollback", a, b, c, from);

killAgent();
console.log("PASS");
