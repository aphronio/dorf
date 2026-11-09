// S1 proof: one Thread, one Sandbox, pause and resume, an agent killed mid-call, delete.
// Needs a cluster prepared by deploy/up.sh and `kubectl ate` on PATH. Uses the faux model.
import { type ChildProcess, spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Sandboxes, SandboxNotFound } from "../src/sandbox.ts";

const PORT = 8193;
const IDLE_MS = 10_000;
const DATA = mkdtempSync(join(tmpdir(), "dorf-s1-"));
const URL_ = `http://127.0.0.1:${PORT}`;
const sandboxes = new Sandboxes();
const t0 = Date.now();

function log(step: string, detail = ""): void {
	console.log(`${String(((Date.now() - t0) / 1000).toFixed(1)).padStart(6)}s  ${step}${detail ? `  ${detail}` : ""}`);
}

let sandbox: string | undefined;
async function fail(message: string): Promise<never> {
	console.error(`FAIL: ${message}`);
	agent?.kill("SIGKILL");
	if (sandbox) await sandboxes.delete(sandbox).catch(() => {});
	process.exit(1);
}

let agent: ChildProcess | undefined;
async function startAgent(): Promise<void> {
	agent = spawn("node", [new URL("../src/main.ts", import.meta.url).pathname], {
		env: { ...process.env, DORF_MODEL: "faux", DORF_DATA: DATA, DORF_PORT: String(PORT), DORF_IDLE_MS: String(IDLE_MS) },
		stdio: ["ignore", "ignore", "inherit"],
	});
	await until("agent up", async () => (await fetch(`${URL_}/threads`).catch(() => undefined))?.ok);
}

async function until<T>(what: string, check: () => Promise<T | undefined | false>, timeoutMs = 60_000): Promise<T> {
	const end = Date.now() + timeoutMs;
	while (Date.now() < end) {
		const value = await check();
		if (value) return value;
		await new Promise((r) => setTimeout(r, 250));
	}
	return await fail(`timed out waiting for ${what}`);
}

async function call(method: string, path: string, body?: unknown): Promise<{ status: number; json: any }> {
	const res = await fetch(URL_ + path, { method, body: body === undefined ? undefined : JSON.stringify(body) });
	const text = await res.text();
	return { status: res.status, json: text ? JSON.parse(text) : undefined };
}

type Entry = { kind: string; model?: { role: string; content?: { type: string; text?: string }[] }[] };
const text = (e: Entry) => (e.model ?? []).flatMap((m) => m.content ?? []).map((c) => c.text ?? "").join("");

// Waits for the run that answers the `n`-th message to end; returns that run's tool outputs.
async function runEnded(id: number, n: number): Promise<string[]> {
	const view = await until(`run ${n} to end`, async () => {
		const { json } = await call("GET", `/threads/${id}`);
		const entries: Entry[] = json.entries;
		const answers = entries.filter((e) => e.kind === "pi.assistant" && text(e) === "done").length;
		return !json.docs["pi.live"]?.run && answers >= n && entries;
	});
	const lastUser = view.findLastIndex((e) => e.kind === "pi.user");
	return view.slice(lastUser).filter((e) => e.kind === "pi.tool-result").map(text);
}

await startAgent();
log("agent started", `data ${DATA}`);

// 1. A tool call writes a file and starts a background process; the Sandbox reaches the internet.
const created = await call("POST", "/threads", {
	prompt: "$ echo hello > note.txt && (sleep 100000 & echo $! > pid) && echo started\n$ git ls-remote https://github.com/git/git HEAD",
});
if (created.status !== 201) await fail(`create: ${created.status} ${JSON.stringify(created.json)}`);
const id: number = created.json.id;
sandbox = created.json.sandbox as string;
const first = await runEnded(id, 1);
if (!first[0]?.includes("started")) await fail(`first tool call: ${JSON.stringify(first)}`);
if (!/\tHEAD/.test(first[1] ?? "")) await fail(`internet: ${JSON.stringify(first[1])}`);
log("1. thread created, file written, process started, internet reached", `thread ${id}, sandbox ${sandbox}`);

// 2. The Sandbox pauses after the idle window.
const ranAt = Date.now();
await until("pause", async () => (await sandboxes.state(sandbox)) === "ACTOR_STATE_SUSPENDED", IDLE_MS + 60_000);
log("2. sandbox paused", `${((Date.now() - ranAt) / 1000).toFixed(1)}s after the run (idle window ${IDLE_MS / 1000}s)`);

// 3. The next message resumes it with the file and the process intact.
const sentAt = Date.now();
await call("POST", `/threads/${id}/messages`, { text: "$ cat note.txt && kill -0 $(cat pid) && echo alive" });
const second = await runEnded(id, 2);
if (!/hello[\s\S]*alive/.test(second[0] ?? "")) await fail(`after resume: ${JSON.stringify(second)}`);
log("3. resumed with file and process intact", `message to answer ${Date.now() - sentAt} ms`);

// 4. An agent killed mid-call finishes the run after a restart.
await call("POST", `/threads/${id}/messages`, { text: "$ sleep 5 && echo slow-done" });
await new Promise((r) => setTimeout(r, 2_000));
agent!.kill("SIGKILL");
log("4. agent killed mid-call");
await startAgent();
const third = await runEnded(id, 3);
if (!third[0]?.includes("interrupted")) await fail(`killed call: ${JSON.stringify(third)}`);
log("   run finished after restart", "the model got the call as interrupted");

// 5. Deleting the Thread deletes its Sandbox.
const deleted = await call("DELETE", `/threads/${id}`);
if (deleted.status !== 204) await fail(`delete: ${deleted.status}`);
const gone = await sandboxes.state(sandbox).then(
	() => false,
	(e) => e instanceof SandboxNotFound,
);
if (!gone) await fail(`sandbox ${sandbox} still exists after delete`);
if ((await call("GET", `/threads/${id}`)).status !== 404) await fail("deleted thread still readable");
log("5. thread deleted, sandbox gone");

agent!.kill();
console.log("PASS");
