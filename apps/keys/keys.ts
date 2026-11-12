import http from "node:http";
import * as claude from "./claude.ts";
import * as github from "./github.ts";
import { Refused } from "./kube.ts";

// Connects the owner's accounts to the broker: the Claude plan and GitHub. The chat page drives it through the agent,
// the only caller its NetworkPolicy admits, and no route returns a token.

const PORT = 8092;

// The one field a route reads from a request.
export type Body = { code?: string };

const ROUTES = new Map(
	Object.entries({
		"GET /claude": claude.status,
		"POST /claude/start": claude.start,
		"POST /claude/code": claude.code,
		"POST /claude/disconnect": claude.disconnect,
		"GET /github": github.status,
		"POST /github/start": github.start,
		"POST /github/disconnect": github.disconnect,
	}),
);

async function body(req: http.IncomingMessage): Promise<Body> {
	let text = "";
	for await (const chunk of req) if ((text += chunk).length > 4096) throw new Refused("too long");
	let json;
	try {
		json = JSON.parse(text || "{}");
	} catch {
		throw new Refused("send JSON");
	}
	// oxlint-disable-next-line anti-slop/no-runtime-typeof -- this parses the request where it enters, and the service has no schema library
	return { code: typeof json?.code === "string" ? json.code : undefined };
}

http
	.createServer(async (req, res) => {
		const send = (status: number, json: string) => res.writeHead(status, { "content-type": "application/json" }).end(json);
		const route = ROUTES.get(`${req.method} ${new URL(req.url ?? "/", "http://x").pathname}`);
		try {
			if (!route) return send(404, JSON.stringify({ error: "not found" }));
			send(200, JSON.stringify(await route(await body(req))));
		} catch (e) {
			send(e instanceof Refused ? 400 : 500, JSON.stringify({ error: e instanceof Refused ? e.message : "the keys service failed; see its log" }));
			if (!(e instanceof Refused)) console.error((e as Error).message);
		}
	})
	.listen(PORT);
console.log(`dorf keys on port ${PORT}`);
