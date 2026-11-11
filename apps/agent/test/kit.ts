import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { mkdtempSync, writeFileSync } from "node:fs";
import type { Readable } from "node:stream";
import { createInterface } from "node:readline";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Sandboxes } from "../src/sandbox.ts";

export const IDLE_MS = 10_000;
const OWNER = "proof@example.com";
const HEADERS = { "tailscale-user-login": OWNER, "content-type": "application/json" };
const PORT = 8193;
export const DATA = mkdtempSync(join(tmpdir(), "dorf-proof-"));
const TOKEN = join(DATA, "token");
writeFileSync(TOKEN, execFileSync("kubectl", ["-n", "dorf-gate", "create", "token", "dorf-agent", "--audience", "api.ate-system.svc", "--duration", "24h"]));
const forward = spawn("kubectl", ["-n", "dorf-gate", "port-forward", "svc/dorf-gate", "0:80"], { stdio: ["ignore", "pipe", "ignore"] });
process.on("exit", () => {
	forward.kill();
	agent?.kill("SIGKILL");
});
const GATE = await new Promise<string>((resolve) => {
	createInterface({ input: forward.stdout as Readable }).on("line", (line) => {
		const port = line.match(/^Forwarding from 127\.0\.0\.1:(\d+) /)?.[1];
		if (port) resolve(`http://127.0.0.1:${port}`);
	});
});
forward.unref();
(forward.stdout as Readable & { unref(): void }).unref();
export const sandboxes = new Sandboxes(GATE, TOKEN);
const t0 = Date.now();
const owned = new Set<string>();
let agent: ChildProcess | undefined;

export function log(step: string, detail = ""): void {
	console.log(`${((Date.now() - t0) / 1000).toFixed(1).padStart(6)}s  ${step}${detail ? `  ${detail}` : ""}`);
}

export async function fail(message: string): Promise<never> {
	console.error(`FAIL: ${message}`);
	agent?.kill("SIGKILL");
	for (const name of owned) await sandboxes.delete(name).catch(() => {});
	process.exit(1);
}

export async function startAgent(model = "faux", idleMs = IDLE_MS): Promise<void> {
	agent = spawn("node", [new URL("../src/main.ts", import.meta.url).pathname], {
		env: { ...process.env, DORF_MODEL: model, DORF_DATA: DATA, DORF_PORT: String(PORT), DORF_IDLE_MS: String(idleMs), DORF_OWNER: OWNER, DORF_GATE: GATE, DORF_TOKEN_FILE: TOKEN },
		stdio: ["ignore", "inherit", "inherit"],
	});
	await until("agent up", async () => (await fetch(`http://127.0.0.1:${PORT}/threads`, { headers: HEADERS }).catch(() => undefined))?.ok);
	log("agent started", `data ${DATA}`);
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
