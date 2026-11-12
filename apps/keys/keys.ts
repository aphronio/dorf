import { type ChildProcess, spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import http from "node:http";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Connects the owner's Claude plan to the broker: it runs Claude Code's own `claude setup-token` and writes the token
// into the Secret the broker reads, and revokes the token it replaces or disconnects. The chat page drives it through
// the agent, the only caller its NetworkPolicy admits, and no route returns the token.

const PORT = 8092;
const CLAUDE = createRequire(import.meta.url).resolve(`@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}/claude`);
const KUBE = "/var/run/secrets/kubernetes.io/serviceaccount";
const SECRET = "https://kubernetes.default.svc/api/v1/namespaces/dorf-broker/secrets/claude";
const FLOW_MS = 10 * 60_000;
const ANSI = /\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-Za-z]/g;
const LINK = /https:\/\/claude\.com\/cai\/oauth\/authorize\?[^\s"'<>]+/;
const TOKEN = /sk-ant-oat01-[\w-]{80,}(?=\s)/;
const TOKEN_IN_SDS = /Bearer (sk-ant-oat01-[\w-]{80,})/;
const CODE = /^[\w#.~-]{8,512}$/;
// Where Claude Code's own logout revokes a token, and the OAuth client its sign-in link names.
const REVOKE = "https://platform.claude.com/v1/oauth/token/revoke";
const CLIENT = "9d1c250a-e61b-44d9-88ed-5944d1962f5e";

class Refused extends Error {}

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

async function kube(method: string, body?: object): Promise<any> {
	const res = await fetch(SECRET, {
		method,
		headers: { authorization: `Bearer ${readFileSync(`${KUBE}/token`, "utf8")}`, "content-type": "application/merge-patch+json" },
		body: body && JSON.stringify(body),
	});
	if (!res.ok) throw new Error(`Secret dorf-broker/claude: ${method} ${res.status}`);
	return res.json();
}

async function connected(): Promise<string | null> {
	return (await kube("GET")).metadata?.annotations?.["dorf.dev/connected"] ?? null;
}

// The broker's credential injector reads an SDS file and reloads it when the Secret changes. No token is "Bearer none".
async function store(token: string | null): Promise<void> {
	const sds = { resources: [{ "@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret", name: "claude", generic_secret: { secret: { inline_string: `Bearer ${token ?? "none"}` } } }] };
	await kube("PATCH", { metadata: { annotations: { "dorf.dev/connected": token && new Date().toISOString() } }, data: { "sds.yaml": Buffer.from(JSON.stringify(sds)).toString("base64") } });
}

async function stored(): Promise<string | undefined> {
	const sds = Buffer.from((await kube("GET")).data?.["sds.yaml"] ?? "", "base64").toString();
	return sds.match(TOKEN_IN_SDS)?.[1];
}

async function revoke(token: string | undefined): Promise<void> {
	if (!token) return;
	const res = await fetch(REVOKE, {
		method: "POST",
		headers: { "content-type": "application/json" },
		body: JSON.stringify({ token, token_type_hint: "access_token", client_id: CLIENT }),
		signal: AbortSignal.timeout(10_000),
	});
	if (!res.ok) throw new Error(`Claude's revoke answered ${res.status}`);
}

async function body(req: http.IncomingMessage): Promise<Record<string, unknown>> {
	let text = "";
	for await (const chunk of req) if ((text += chunk).length > 4096) throw new Refused("too long");
	try {
		return JSON.parse(text || "{}");
	} catch {
		throw new Refused("send JSON");
	}
}

async function route(req: http.IncomingMessage): Promise<[number, unknown]> {
	switch (`${req.method} ${new URL(req.url ?? "/", "http://x").pathname}`) {
		case "GET /claude":
			return [200, { connected: await connected() }];
		case "POST /claude/start":
			return [200, { url: await find(begin(), LINK, 20_000, "sign-in link") }];
		case "POST /claude/code": {
			const { code } = await body(req);
			const f = flow;
			if (!f || f.done) throw new Refused("start again");
			if (typeof code !== "string" || !CODE.test(code)) throw new Refused("that does not look like a sign-in code");
			// Typed, then Enter on its own, so the terminal does not read the two as one paste.
			f.child.stdin!.write(code);
			await new Promise((r) => setTimeout(r, 300));
			f.child.stdin!.write("\r");
			const token = await find(f, TOKEN, 30_000, "token for that code");
			end(f);
			// A new token leaves the old one live, so the old one is revoked once the broker has the new one.
			const old = await stored();
			await store(token);
			await revoke(old);
			return [200, { connected: await connected() }];
		}
		case "POST /claude/disconnect":
			await revoke(await stored());
			await store(null);
			return [200, { connected: null }];
	}
	return [404, { error: "not found" }];
}

http
	.createServer(async (req, res) => {
		const send = (status: number, value: unknown) => res.writeHead(status, { "content-type": "application/json" }).end(JSON.stringify(value));
		try {
			send(...(await route(req)));
		} catch (e) {
			send(e instanceof Refused ? 400 : 500, { error: e instanceof Refused ? e.message : "the keys service failed; see its log" });
			if (!(e instanceof Refused)) console.error((e as Error).message);
		}
	})
	.listen(PORT);
console.log(`dorf keys on port ${PORT}`);
