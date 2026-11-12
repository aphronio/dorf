import { type ChildProcess, spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import http from "node:http";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";

// Connects the owner's Claude plan to the broker: it runs Claude Code's own `claude setup-token` and writes the token
// into the Secret the broker reads; no route returns it. It listens on the pod's loopback, so only a
// port-forward behind `tailscale serve` on its own port reaches it: the agent cannot, and the Tailscale login header can
// be trusted. Its own origin keeps the agent's page from driving it.

const PORT = 8092;
const OWNER = process.env.DORF_OWNER;
if (!OWNER) throw new Error("set DORF_OWNER to the Tailscale login allowed to connect keys");
const CLAUDE = createRequire(import.meta.url).resolve(`@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}/claude`);
const KUBE = "/var/run/secrets/kubernetes.io/serviceaccount";
const SECRET = "https://kubernetes.default.svc/api/v1/namespaces/dorf-broker/secrets/claude";
const FLOW_MS = 10 * 60_000;
const ANSI = /\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][0-9A-Za-z]/g;
const LINK = /https:\/\/claude\.com\/cai\/oauth\/authorize\?[^\s"'<>]+/;
const TOKEN = /sk-ant-oat01-[\w-]{80,}(?=\s)/;
const CODE = /^[\w#.~-]{8,512}$/;
const PAGE = { "/": ["page.html", "text/html; charset=utf-8"], "/page.js": ["page.js", "text/javascript"] } as const;
const HEADERS = {
	"content-security-policy": "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'",
	"x-content-type-options": "nosniff",
	"referrer-policy": "no-referrer",
	"cache-control": "no-store",
};

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

// The broker's credential injector reads an SDS file and reloads it when the Secret changes.
async function store(token: string): Promise<void> {
	const sds = { resources: [{ "@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret", name: "claude", generic_secret: { secret: { inline_string: `Bearer ${token}` } } }] };
	await kube("PATCH", { metadata: { annotations: { "dorf.dev/connected": new Date().toISOString() } }, data: { "sds.yaml": Buffer.from(JSON.stringify(sds)).toString("base64") } });
}

async function body(req: http.IncomingMessage): Promise<Record<string, unknown>> {
	if (req.headers["content-type"] !== "application/json") throw new Refused("send JSON");
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
			await store(token);
			return [200, { connected: await connected() }];
		}
	}
	return [404, { error: "not found" }];
}

http
	.createServer(async (req, res) => {
		const send = (status: number, value: unknown) => res.writeHead(status, { ...HEADERS, "content-type": "application/json" }).end(JSON.stringify(value));
		try {
			if (req.headers["tailscale-user-login"] !== OWNER) return send(403, { error: "not the owner" });
			const page = req.method === "GET" ? PAGE[new URL(req.url ?? "/", "http://x").pathname as keyof typeof PAGE] : undefined;
			if (page) return res.writeHead(200, { ...HEADERS, "content-type": page[1] }).end(readFileSync(new URL(page[0], import.meta.url)));
			if (req.method !== "GET" && req.headers["sec-fetch-site"] !== "same-origin") return send(403, { error: "only this page may connect keys" });
			send(...(await route(req)));
		} catch (e) {
			send(e instanceof Refused ? 400 : 500, { error: e instanceof Refused ? e.message : "the keys service failed; see its log" });
			if (!(e instanceof Refused)) console.error((e as Error).message);
		}
	})
	.listen(PORT, "127.0.0.1");
console.log(`dorf keys on 127.0.0.1:${PORT}`);
