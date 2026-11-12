import { type ChildProcess, spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { oneAtATime, Refused, sds, secret } from "./kube.ts";

// The Claude plan: Claude Code's own `claude setup-token` makes a one-year token, which goes into the Secret the broker
// reads; the token it replaces, or disconnects, is revoked.

const CLAUDE = createRequire(import.meta.url).resolve(`@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}/claude`);
const FLOW_MS = 10 * 60_000;
const ANSI = /\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-Za-z]/g;
const LINK = /https:\/\/claude\.com\/cai\/oauth\/authorize\?[^\s"'<>]+/;
const TOKEN = /sk-ant-oat01-[\w-]{80,}(?=\s)/;
const TOKEN_IN_SDS = /Bearer (sk-ant-oat01-[\w-]{80,})/;
const CODE = /^[\w#.~-]{8,512}$/;
// Where Claude Code's own logout revokes a token, and the OAuth client its sign-in link names.
const REVOKE = "https://platform.claude.com/v1/oauth/token/revoke";
const CLIENT = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";

type Flow = { child: ChildProcess; home: string; out: string; done: boolean };
let flow: Flow | undefined;

function end(f = flow) {
	if (!f) return;
	f.child.kill();
	rmSync(f.home, { recursive: true, force: true });
	if (flow === f) flow = undefined;
}

// Claude Code prints the link and the token to a terminal only, so it gets a wide one that never wraps them.
function begin(): Flow {
	end();
	const home = mkdtempSync(join(tmpdir(), "claude-"));
	const child = spawn("script", ["-qfec", `stty cols 1000; exec ${CLAUDE} setup-token`, "/dev/null"], {
		env: { PATH: process.env.PATH, HOME: home, TERM: "xterm-256color", BROWSER: "/bin/true" },
		stdio: ["pipe", "pipe", "inherit"],
	});
	const f: Flow = { child, home, out: "", done: false };
	child.stdout!.on("data", (chunk: Buffer) => void (f.out += chunk));
	child.once("exit", () => void (f.done = true)).on("error", () => void (f.done = true));
	child.stdin!.on("error", () => {});
	setTimeout(() => end(f), FLOW_MS).unref();
	return (flow = f);
}

async function find(f: Flow, pattern: RegExp, ms: number, what: string): Promise<string> {
	for (const until = Date.now() + ms; Date.now() < until; await new Promise((r) => setTimeout(r, 200))) {
		const hit = f.out.replace(ANSI, "").match(pattern)?.[0];
		if (hit) return hit;
		if (f.done || flow !== f) break;
	}
	end(f);
	throw new Refused(`Claude Code gave no ${what}; start again`);
}

async function connected(): Promise<string | null> {
	return (await secret("dorf-broker", "claude")).metadata?.annotations?.["dorf.dev/connected"] ?? null;
}

// No token is "Bearer none".
async function store(token: string | null): Promise<void> {
	await secret("dorf-broker", "claude", { metadata: { annotations: { "dorf.dev/connected": token && new Date().toISOString() } }, data: { "sds.yaml": sds("claude", `Bearer ${token ?? "none"}`) } });
}

async function stored(): Promise<string | undefined> {
	const sds = Buffer.from((await secret("dorf-broker", "claude")).data?.["sds.yaml"] ?? "", "base64").toString();
	return sds.match(TOKEN_IN_SDS)?.[1];
}

async function revoke(token: string | undefined): Promise<void> {
	if (!token) return;
	const res = await fetch(REVOKE, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: JSON.stringify({ token, token_type_hint: "access_token", client_id: CLIENT }),
		redirect: "error",
		signal: AbortSignal.timeout(10_000),
	});
	// As Claude Code counts it: a 2xx without Anthropic's request id did not come from Anthropic.
	if (!res.ok || !res.headers.get("request-id")) throw new Error(`Claude's revoke answered ${res.status}`);
}

const inTurn = oneAtATime();

// The old token is revoked before the new one is stored, so neither is left live outside the Secret: if either step
// fails, the new token is revoked and the old one stays connected.
async function replace(token: string, old: string | undefined): Promise<void> {
	try {
		if (old !== token) await revoke(old);
		await store(token);
	} catch (e) {
		await revoke(token).catch((r) => console.error(`a new token may be live outside the Secret: ${(r as Error).message}`));
		console.error((e as Error).message);
		throw new Refused("Claude did not take the change; start again");
	}
}

export async function status() {
	return { connected: await connected() };
}

export async function start() {
	return { url: await find(begin(), LINK, 20_000, "sign-in link") };
}

export async function code(body: Record<string, unknown>) {
	const f = flow;
	if (!f || f.done) throw new Refused("start again");
	if (typeof body.code !== "string" || !CODE.test(body.code)) throw new Refused("that does not look like a sign-in code");
	// Typed, then Enter on its own, so the terminal does not read the two as one paste.
	f.child.stdin!.write(body.code);
	await new Promise((r) => setTimeout(r, 300));
	f.child.stdin!.write("\r");
	const token = await find(f, TOKEN, 30_000, "token for that code");
	end(f);
	// A new token leaves the old one live.
	await inTurn(async () => replace(token, await stored()));
	return { connected: await connected() };
}

export async function disconnect() {
	await inTurn(async () => {
		await revoke(await stored());
		await store(null);
	});
	return { connected: null };
}
