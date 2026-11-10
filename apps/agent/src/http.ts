import { readFile } from "node:fs/promises";
import http from "node:http";
import { extname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { ConversationId } from "@earendil-works/pi-durable";
import { type ClaudeLogin, LoginError } from "./claude-login.ts";
import type { GitHub } from "./github.ts";
import type { Threads } from "./threads.ts";

type Reply = [status: number, body: unknown];

// The page's static export, copied in by the image build (apps/web).
const WEB = fileURLToPath(new URL("../web/", import.meta.url));
const API = new Set(["threads", "repos", "claude"]);
const TYPES: Record<string, string> = {
	".html": "text/html; charset=utf-8",
	".txt": "text/plain; charset=utf-8",
	".js": "text/javascript",
	".css": "text/css",
	".woff2": "font/woff2",
	".ico": "image/x-icon",
	".svg": "image/svg+xml",
	".png": "image/png",
};
const MAX_BODY = 1 << 20;
// Identity comes from the network, not a cookie, so a framing site would act as the owner.
const HEADERS = { "cache-control": "no-store", "x-frame-options": "DENY", "content-security-policy": "frame-ancestors 'none'", "x-content-type-options": "nosniff" };

class BadRequest extends Error {}

async function json(req: http.IncomingMessage): Promise<Record<string, unknown>> {
	let text = "";
	for await (const chunk of req) {
		text += chunk;
		if (text.length > MAX_BODY) throw new BadRequest("body too large");
	}
	try {
		return text ? JSON.parse(text) : {};
	} catch {
		throw new BadRequest("body is not JSON");
	}
}

// Only `tailscale serve` reaches the agent's loopback, and it sets the caller's tailnet login. A
// change must be JSON, so a browser sends another site's request only after a preflight we never answer.
function refused(req: http.IncomingMessage, owner: string): Reply | undefined {
	if (req.headers["tailscale-user-login"] !== owner) return [403, { error: "not the owner" }];
	if (req.method === "GET") return undefined;
	if (!req.headers["content-type"]?.startsWith("application/json")) return [415, { error: "JSON only" }];
	const site = req.headers["sec-fetch-site"];
	if (site && site !== "same-origin" && site !== "none") return [403, { error: "cross-site request" }];
}

async function route(threads: Threads, claude: ClaudeLogin, github: GitHub | undefined, req: http.IncomingMessage): Promise<Reply> {
	const [, root, rawId, sub] = new URL(req.url ?? "/", "http://x").pathname.split("/");
	if (root === "repos" && req.method === "GET" && !rawId) return [200, github ? await github.repos() : []];
	if (root === "claude") {
		switch (`${req.method} ${rawId ?? ""}`) {
			case "GET ":
				return [200, await claude.status()];
			case "POST login":
				return [200, { url: await claude.start() }];
			case "POST code":
				return [200, await claude.finish((await json(req)).code)];
		}
		return [405, { error: "method not allowed" }];
	}
	if (root !== "threads") return [404, { error: "not found" }];
	const id = Number(rawId) as ConversationId;
	const at = `${req.method} ${[rawId && ":id", sub].filter(Boolean).join("/")}`;
	switch (at) {
		case "GET ":
			return [200, await threads.list()];
		case "POST ": {
			const { prompt, repo } = await json(req);
			if (typeof prompt !== "string" || !prompt) return [400, { error: "prompt required" }];
			if (repo !== undefined && typeof repo !== "string") return [400, { error: "repo must be owner/name" }];
			return [201, await threads.create(prompt, repo || undefined)];
		}
		case "GET :id": {
			const view = await threads.read(id);
			return view ? [200, { sandbox: await threads.sandbox(id), ...view }] : [404, { error: "no such thread" }];
		}
		case "POST :id/messages": {
			const { text } = await json(req);
			if (typeof text !== "string" || !text) return [400, { error: "text required" }];
			return (await threads.send(id, text)) ? [202, {}] : [404, { error: "no such thread" }];
		}
		case "POST :id/abort":
			return (await threads.abort(id)) ? [202, {}] : [404, { error: "no such thread" }];
		case "POST :id/compact":
			return (await threads.compact(id)) ? [202, {}] : [404, { error: "no such thread" }];
		case "DELETE :id":
			return (await threads.delete(id)) ? [204, null] : [404, { error: "no such thread" }];
	}
	return [405, { error: "method not allowed" }];
}

async function asset(path: string, res: http.ServerResponse): Promise<boolean> {
	const file = join(WEB, path === "/" ? "index.html" : decodeURIComponent(path));
	if (!file.startsWith(WEB)) return false;
	const body = await readFile(file).catch(() => undefined);
	if (!body) return false;
	const cache = path.startsWith("/_next/static/") ? "public, max-age=31536000, immutable" : "no-store";
	res.writeHead(200, { ...HEADERS, "cache-control": cache, "content-type": TYPES[extname(file)] ?? "application/octet-stream" }).end(body);
	return true;
}

async function follow(threads: Threads, id: ConversationId, req: http.IncomingMessage, res: http.ServerResponse): Promise<boolean> {
	const stop = await threads.follow(id, (event) => {
		if (!res.headersSent) res.writeHead(200, { ...HEADERS, "cache-control": "no-store, no-transform", "content-type": "text/event-stream" });
		res.write(`data: ${JSON.stringify(event)}\n\n`);
	});
	if (!stop) return false;
	req.once("close", () => void stop());
	return true;
}

export function serve(threads: Threads, claude: ClaudeLogin, github: GitHub | undefined, owner: string, port: number): http.Server {
	return http
		.createServer(async (req, res) => {
			const send = ([status, body]: Reply) =>
				res.writeHead(status, { ...HEADERS, "content-type": "application/json" }).end(status === 204 ? undefined : JSON.stringify(body));
			try {
				const denied = refused(req, owner);
				if (denied) return send(denied);
				const path = new URL(req.url ?? "/", "http://x").pathname;
				if (req.method === "GET" && !API.has(path.split("/")[1])) return (await asset(path, res)) || send([404, { error: "not found" }]);
				const events = req.method === "GET" && path.match(/^\/threads\/(\d+)\/events$/);
				if (events) return (await follow(threads, Number(events[1]) as ConversationId, req, res)) || send([404, { error: "no such thread" }]);
				send(await route(threads, claude, github, req));
			} catch (e) {
				send([e instanceof BadRequest || e instanceof LoginError || e instanceof RangeError ? 400 : 500, { error: (e as Error).message }]);
			}
		})
		.listen(port, "127.0.0.1");
}
