import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Sandboxes } from "../src/sandbox.ts";
import { localGate } from "./local-gate.ts";

const OWNER = "proof@example.com";
export const HEADERS = { "tailscale-user-login": OWNER, "content-type": "application/json" };
// Free when the proof starts and kept across agent restarts, so proofs can run at once.
const PORT = await new Promise<number>((resolve) => {
	const probe = createServer().listen(0, "127.0.0.1", () => {
		const { port } = probe.address() as AddressInfo;
		probe.close(() => resolve(port));
	});
});
export const AGENT = `http://127.0.0.1:${PORT}`;
// As many Sandboxes as the lab's worker holds at once.
const LOCAL_SLOTS = 3;
export const DATA = mkdtempSync(join(tmpdir(), "dorf-proof-"));
// The agent's own settings name a real gate (deploy/proof.sh sets them); without them, Sandboxes are local stand-ins.
export const substrate = Boolean(process.env.DORF_GATE && process.env.DORF_TOKEN_FILE);
// Stand-ins pause at once, so their window only needs to outlast a step; the real gate pauses asynchronously.
export const IDLE_MS = substrate ? 10_000 : 3_000;
const GATE = substrate ? process.env.DORF_GATE! : await localGate(LOCAL_SLOTS, join(DATA, "sandboxes"));
const TOKEN = substrate ? process.env.DORF_TOKEN_FILE! : join(DATA, "token");
if (!substrate) writeFileSync(TOKEN, "local");
// A run that passes removes its folder (pass()); any other end keeps it to inspect.
process.on("exit", (code) => {
	agent?.kill("SIGKILL");
	if (code !== 0) console.error(`kept ${DATA} to inspect`);
});
// Proof Sandboxes start "p-"; deploy/proof.sh deletes those a killed run left.
const PREFIX = "p-";
export const sandboxes = new Sandboxes(GATE, TOKEN, PREFIX);
const t0 = Date.now();
const owned = new Set<string>();
let agent: ChildProcess | undefined;

console.log(substrate ? `Sandboxes on Substrate through ${GATE}` : `Sandboxes are local stand-ins, each in its own folder on one machine: this proves the agent, not Substrate (test/substrate.ts)`);

// For claims only Substrate can make good on.
export async function needSubstrate(): Promise<void> {
	if (!substrate) await fail("this proof needs Substrate: run it through deploy/proof.sh");
}

export function log(step: string, detail = ""): void {
	console.log(`${((Date.now() - t0) / 1000).toFixed(1).padStart(6)}s  ${step}${detail ? `  ${detail}` : ""}`);
}

// Ends a proof that held: stops the agent, removes the run's folder and exits 0.
export async function pass(): Promise<never> {
	if (agent && agent.exitCode === null && agent.signalCode === null) {
		const exited = new Promise((resolve) => agent!.once("exit", resolve));
		agent.kill("SIGTERM");
		const late = setTimeout(() => agent?.kill("SIGKILL"), 10_000);
		await exited;
		clearTimeout(late);
	}
	rmSync(DATA, { recursive: true, force: true });
	console.log("PASS");
	process.exit(0);
}

export async function fail(message: string): Promise<never> {
	console.error(`FAIL: ${message}`);
	agent?.kill("SIGKILL");
	for (const name of owned) await sandboxes.delete(name).catch(() => {});
	process.exit(1);
}

// On stand-ins the workspace is /proc/self/cwd: each Sandbox's pi-env starts in that Sandbox's own folder, and
// every command it runs resolves the path there, so files stay apart with no change to the agent or the gate.
export async function startAgent(model = "faux", idleMs = IDLE_MS, env: Record<string, string> = {}): Promise<void> {
	agent = spawn("node", [new URL("../src/main.ts", import.meta.url).pathname], {
		env: { ...process.env, DORF_MODEL: model, DORF_DATA: DATA, DORF_PORT: String(PORT), DORF_IDLE_MS: String(idleMs), DORF_OWNER: OWNER, DORF_GATE: GATE, DORF_TOKEN_FILE: TOKEN, DORF_SANDBOX_PREFIX: PREFIX, ...env, ...(substrate ? {} : { DORF_WORKSPACE: "/proc/self/cwd", DORF_JOBS: join(DATA, "jobs") }) },
		stdio: ["ignore", "inherit", "inherit"],
	});
	await until("agent up", async () => (await fetch(`http://127.0.0.1:${PORT}/threads`, { headers: HEADERS }).catch(() => undefined))?.ok);
	log("agent started", `data ${DATA}`);
}

// Repositories on this machine that the stand-ins' git clones in place of github.com's: `files`, with an executable
// `.agents/setup` if given, committed to a bare repository. `commit` adds a commit there.
const REPOS = join(DATA, "repos");
if (!substrate) Object.assign(process.env, { GIT_CONFIG_COUNT: "1", GIT_CONFIG_KEY_0: `url.${REPOS}/.insteadOf`, GIT_CONFIG_VALUE_0: "https://github.com/" });
export function repository(name: string, files: Record<string, string>): { repo: string; commit: (files: Record<string, string>, message: string) => string } {
	const dir = join(REPOS, "work", name);
	const git = (...args: string[]) => execFileSync("git", ["-c", "user.name=proof", "-c", "user.email=proof@example.com", ...args], { cwd: dir, encoding: "utf8" }).trim();
	const commit = (changes: Record<string, string>, message: string) => {
		for (const [path, text] of Object.entries(changes)) {
			mkdirSync(join(dir, path, ".."), { recursive: true });
			writeFileSync(join(dir, path), text);
			if (path === ".agents/setup") chmodSync(join(dir, path), 0o755);
		}
		git("add", ".");
		git("commit", "-qm", message);
		git("push", "-q", join(REPOS, "proof", `${name}.git`), "HEAD:main");
		return git("rev-parse", "HEAD");
	};
	mkdirSync(dir, { recursive: true });
	git("init", "-q", "-b", "main");
	execFileSync("git", ["init", "-q", "--bare", "-b", "main", join(REPOS, "proof", `${name}.git`)]);
	commit(files, "first");
	return { repo: `proof/${name}`, commit };
}

export function agentPid(): number | undefined {
	return agent?.pid;
}

export function killAgent(signal: NodeJS.Signals = "SIGTERM"): void {
	agent?.kill(signal);
}

export async function until<T>(what: string, check: () => Promise<T | undefined | false>, timeoutMs = 60_000): Promise<T> {
	const end = Date.now() + timeoutMs;
	while (Date.now() < end) {
		const value = await check();
		if (value) return value;
		await new Promise((r) => setTimeout(r, 250));
	}
	return await fail(`timed out waiting for ${what}`);
}

export async function call(method: string, path: string, body?: unknown): Promise<{ status: number; json: any }> {
	const res = await fetch(`http://127.0.0.1:${PORT}${path}`, { method, headers: HEADERS, body: body === undefined ? undefined : JSON.stringify(body) });
	const text = await res.text();
	return { status: res.status, json: text ? JSON.parse(text) : undefined };
}

export async function createThread(prompt: string, repo?: string, choice?: { model?: string; thinking?: string }): Promise<{ id: number; sandbox: string }> {
	const created = await call("POST", "/threads", { prompt, repo, ...choice });
	if (created.status !== 201) await fail(`create: ${created.status} ${JSON.stringify(created.json)}`);
	owned.add(created.json.sandbox);
	return created.json;
}

type Content = { type: string; text?: string };
type Entry = { kind: string; model?: { role: string; isError?: boolean; content?: Content[] | string }[] };
const text = (e: Entry) =>
	(e.model ?? []).flatMap((m) => (typeof m.content === "string" ? [{ type: "text", text: m.content }] : (m.content ?? []))).map((c) => c.text ?? "").join("");

export type Result = { output: string; error: boolean };

export async function runEnded(id: number, message: string, timeoutMs = 60_000): Promise<Result[]> {
	return until(
		`the answer to ${JSON.stringify(message.slice(0, 40))}`,
		async () => {
			const { json } = await call("GET", `/threads/${id}`);
			const entries: Entry[] = json.entries;
			const asked = entries.findLastIndex((e) => e.kind === "pi.user");
			if (json.docs["pi.live"]?.run || asked < 0 || text(entries[asked]) !== message) return undefined;
			const answer = entries.slice(asked);
			if (!answer.some((e) => e.kind === "pi.assistant" && text(e) === "done")) return undefined;
			return answer.filter((e) => e.kind === "pi.tool-result").map((e) => ({ output: text(e), error: Boolean(e.model?.[0]?.isError) }));
		},
		timeoutMs,
	);
}

export async function send(id: number, message: string): Promise<void> {
	const sent = await call("POST", `/threads/${id}/messages`, { text: message });
	if (sent.status !== 202) await fail(`send: ${sent.status}`);
}

// A Thread's event stream, as the page reads it; each event carries its SSE id. A reconnect sends the last one.
export function follow(path: string, lastEventId?: number): { events: any[]; close: () => void } {
	const events: any[] = [];
	const abort = new AbortController();
	const headers = lastEventId === undefined ? HEADERS : { ...HEADERS, "last-event-id": String(lastEventId) };
	void fetch(`http://127.0.0.1:${PORT}${path}`, { headers, signal: abort.signal })
		.then(async (res) => {
			let buffer = "";
			for await (const chunk of res.body!.pipeThrough(new TextDecoderStream())) {
				buffer += chunk;
				const parts = buffer.split("\n\n");
				buffer = parts.pop()!;
				for (const part of parts) {
					const field = (name: string) => part.split("\n").find((l) => l.startsWith(`${name}: `))?.slice(name.length + 2);
					const data = field("data");
					if (data) events.push({ ...JSON.parse(data), ...(field("id") ? { id: Number(field("id")) } : {}) });
				}
			}
		})
		.catch(() => {});
	return { events, close: () => abort.abort() };
}
