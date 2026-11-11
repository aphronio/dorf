import { readFile } from "node:fs/promises";
import http from "node:http";
import { extname, join } from "node:path";
import { fileURLToPath } from "node:url";
import type { ImageContent } from "@earendil-works/pi-ai";
import type { ConversationId, EntryId, UserInput } from "@earendil-works/pi-durable";
import type { ImageProcessor } from "@earendil-works/pi-durable/tools";
import { type ClaudeLogin, LoginError } from "./claude-login.ts";
import type { GitHub } from "./github.ts";
import { prepareImages } from "./images.ts";
import type { Catalog } from "./models.ts";
import type { Threads } from "./threads.ts";

type Reply = [status: number, body: unknown];

// The page's static export, copied in by the image build (apps/web).
const WEB = fileURLToPath(new URL("../web/", import.meta.url));
const API = new Set(["threads", "repos", "claude", "models"]);
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
// A message may carry images: their base64 within prepareImages' total, before it shrinks them.
const MAX_MESSAGE_BODY = 16 << 20;
// Identity comes from the network, not a cookie, so a framing site would act as the owner. A reply is text from a
// model that reads the internet, so the page loads no image from another origin: an image URL would carry data out.
// A blob: URL names bytes the page already holds, such as an image the owner is about to send, so it carries nothing out.
// A reply's views run in sandboxed frames, and frame-src keeps a frame from loading a page from another origin.
const HEADERS = { "cache-control": "no-store", "x-frame-options": "DENY", "content-security-policy": "frame-ancestors 'none'; frame-src 'self'; img-src 'self' data: blob:", "x-content-type-options": "nosniff" };

class BadRequest extends Error {
	readonly status: number;
	constructor(message: string, status = 400) {
		super(message);
		this.status = status;
	}
}

// A body past the limit is still read to its end, so the browser gets the answer instead of a reset connection.
async function json(req: http.IncomingMessage, limit = MAX_BODY): Promise<Record<string, unknown>> {
	const tooLarge = new BadRequest(`body larger than ${limit >> 20} MiB`, 413);
	if (Number(req.headers["content-length"]) > limit) throw tooLarge;
	const chunks: Buffer[] = [];
	let size = 0;
	for await (const chunk of req as AsyncIterable<Buffer>) {
		size += chunk.length;
		if (size <= limit) chunks.push(chunk);
	}
	if (size > limit) throw tooLarge;
	try {
		return size ? JSON.parse(Buffer.concat(chunks).toString("utf8")) : {};
	} catch {
		throw new BadRequest("body is not JSON");
	}
}

// The images of a message, as the page sends them: base64 strings.
function uploads(value: unknown): string[] {
	if (value === undefined) return [];
	if (!Array.isArray(value) || !value.every((v) => typeof v === "string")) throw new BadRequest("images must be base64 strings");
	return value;
}

// Text alone stays a string, as before images; with images, Pi's content blocks, the text first.
const input = (text: string, images: ImageContent[]): UserInput => (images.length ? [...(text ? [{ type: "text" as const, text }] : []), ...images] : text);

// Only `tailscale serve` reaches the agent's loopback, and it sets the caller's tailnet login. A
// change must be JSON, so a browser sends another site's request only after a preflight we never answer.
function refused(req: http.IncomingMessage, owner: string): Reply | undefined {
	if (req.headers["tailscale-user-login"] !== owner) return [403, { error: "not the owner" }];
	if (req.method === "GET") return undefined;
	if (!req.headers["content-type"]?.startsWith("application/json")) return [415, { error: "JSON only" }];
	const site = req.headers["sec-fetch-site"];
	if (site && site !== "same-origin" && site !== "none") return [403, { error: "cross-site request" }];
}

async function route(threads: Threads, catalog: Catalog, claude: ClaudeLogin, github: GitHub | undefined, images: ImageProcessor, req: http.IncomingMessage): Promise<Reply> {
	const [, root, rawId, sub] = new URL(req.url ?? "/", "http://x").pathname.split("/");
	if (root === "repos" && req.method === "GET" && !rawId) return [200, github ? await github.repos() : []];
	if (root === "models" && req.method === "GET" && !rawId) return [200, catalog.list()];
	if (root === "claude") {
		switch (`${req.method} ${rawId ?? ""}`) {
			case "GET ":
				return [200, await claude.status()];
			case "POST login":
				return [200, { url: await claude.start() }];
			case "POST code": {
				const status = await claude.finish((await json(req)).code);
				await catalog.refresh();
				return [200, status];
			}
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
			const { prompt = "", repo, model, thinking, images: attached } = await json(req, MAX_MESSAGE_BODY);
			if (typeof prompt !== "string") return [400, { error: "prompt must be text" }];
			const list = uploads(attached);
			if (!prompt && !list.length) return [400, { error: "prompt or images required" }];
			if (repo !== undefined && typeof repo !== "string") return [400, { error: "repo must be owner/name" }];
			const choice = catalog.choose(model, thinking);
			const prepared = await prepareImages(images, catalog.imageLimits(choice.model), list);
			return [201, await threads.create(input(prompt, prepared), prompt || "Image", repo || undefined, choice)];
		}
		case "GET :id": {
			const view = await threads.read(id);
			return view ? [200, { sandbox: await threads.sandbox(id), agent: await threads.agent(id), ...view }] : [404, { error: "no such thread" }];
		}
		case "POST :id/messages": {
			const { text = "", images: attached } = await json(req, MAX_MESSAGE_BODY);
			if (typeof text !== "string") return [400, { error: "text must be text" }];
			const list = uploads(attached);
			if (!text && !list.length) return [400, { error: "text or images required" }];
			const agent = await threads.agent(id);
			if (!agent) return [404, { error: "no such thread" }];
			const prepared = await prepareImages(images, catalog.imageLimits(agent.model), list);
			return (await threads.send(id, input(text, prepared))) ? [202, {}] : [404, { error: "no such thread" }];
		}
		case "POST :id/thinking": {
			const agent = await threads.agent(id);
			if (!agent) return [404, { error: "no such thread" }];
			if (!agent.model) return [409, { error: "the Thread has no model" }];
			const { level } = await json(req);
			if (typeof level !== "string") return [400, { error: "level required" }];
			return (await threads.think(id, catalog.thinking(agent.model, level))) ? [202, {}] : [404, { error: "no such thread" }];
		}
		case "POST :id/archive":
			return (await threads.archive(id)) ? [202, {}] : [404, { error: "no such thread" }];
		case "POST :id/unarchive":
			return (await threads.unarchive(id)) ? [202, {}] : [404, { error: "no such thread" }];
		case "POST :id/seen":
			return (await threads.seen(id)) ? [202, {}] : [404, { error: "no such thread" }];
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

// Server-sent events. An event with entries takes the newest one's id, which the browser's reconnect sends back as
// Last-Event-ID; a page that opens the stream again itself sends it as `after`, since it cannot set that header.
async function follow<E extends object>(start: (after: EntryId | undefined, emit: (event: E) => void) => Promise<(() => Promise<unknown>) | undefined>, req: http.IncomingMessage, res: http.ServerResponse): Promise<boolean> {
	const last = Number(req.headers["last-event-id"] ?? new URL(req.url ?? "/", "http://x").searchParams.get("after"));
	const stop = await start(Number.isSafeInteger(last) && last > 0 ? (last as EntryId) : undefined, (event) => {
		if (!res.headersSent) res.writeHead(200, { ...HEADERS, "cache-control": "no-store, no-transform", "content-type": "text/event-stream" });
		const newest = (event as { entries?: readonly { id: EntryId }[] }).entries?.at(-1)?.id;
		res.write(`${newest === undefined ? "" : `id: ${newest}\n`}data: ${JSON.stringify(event)}\n\n`);
	});
	if (!stop) return false;
	// The browser may have left while the watches started.
	if (res.closed) void stop();
	else res.once("close", () => void stop());
	return true;
}

export function serve(threads: Threads, catalog: Catalog, claude: ClaudeLogin, github: GitHub | undefined, images: ImageProcessor, owner: string, port: number): http.Server {
	return http
		.createServer(async (req, res) => {
			const send = ([status, body]: Reply) =>
				res.writeHead(status, { ...HEADERS, "content-type": "application/json" }).end(status === 204 ? undefined : JSON.stringify(body));
			try {
				const denied = refused(req, owner);
				if (denied) return send(denied);
				const path = new URL(req.url ?? "/", "http://x").pathname;
				if (req.method === "GET" && !API.has(path.split("/")[1])) return (await asset(path, res)) || send([404, { error: "not found" }]);
				if (req.method === "GET" && path === "/threads/events") return (await follow((_, emit) => threads.followList(emit), req, res)) || send([503, { error: "no thread list" }]);
				const stream = req.method === "GET" && path.match(/^\/threads\/(\d+)(?:\/subagents\/([^/]+))?\/(events|entries)$/);
				if (stream) {
					const id = Number(stream[1]) as ConversationId;
					const name = stream[2] ? decodeURIComponent(stream[2]) : undefined;
					const missing: Reply = [404, { error: name ? "no such subagent" : "no such thread" }];
					if (stream[3] === "events") return (await follow((after, emit) => threads.follow(id, name, after, emit), req, res)) || send(missing);
					const before = Number(new URL(req.url ?? "/", "http://x").searchParams.get("before"));
					if (!Number.isSafeInteger(before) || before < 1) return send([400, { error: "before must be an entry id" }]);
					return send((await threads.earlier(id, name, before as EntryId).then((page) => page && ([200, page] as Reply))) ?? missing);
				}
				send(await route(threads, catalog, claude, github, images, req));
			} catch (e) {
				send([e instanceof BadRequest ? e.status : e instanceof LoginError || e instanceof RangeError ? 400 : 500, { error: (e as Error).message }]);
			}
		})
		.listen(port, "127.0.0.1");
}
