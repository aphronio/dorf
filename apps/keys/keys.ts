import http from "node:http";
import * as claude from "./claude.ts";
import { Refused } from "./kube.ts";

// Connects the owner's accounts to the broker: the Claude plan. The chat page drives it through the agent,
// the only caller its NetworkPolicy admits, and no route returns a token.

const PORT = 8092;

const ROUTES: Record<string, (body: Record<string, unknown>) => Promise<unknown>> = {
	"GET /claude": claude.status,
	"POST /claude/start": claude.start,
	"POST /claude/code": claude.code,
	"POST /claude/disconnect": claude.disconnect,
};

async function body(req: http.IncomingMessage): Promise<Record<string, unknown>> {
	let text = "";
	for await (const chunk of req) if ((text += chunk).length > 4096) throw new Refused("too long");
	try {
		return JSON.parse(text || "{}");
	} catch {
		throw new Refused("send JSON");
	}
}

http
	.createServer(async (req, res) => {
		const send = (status: number, value: unknown) => res.writeHead(status, { "content-type": "application/json" }).end(JSON.stringify(value));
		const route = ROUTES[`${req.method} ${new URL(req.url ?? "/", "http://x").pathname}`];
		try {
			if (!route) return send(404, { error: "not found" });
			send(200, await route(await body(req)));
		} catch (e) {
			send(e instanceof Refused ? 400 : 500, { error: e instanceof Refused ? e.message : "the keys service failed; see its log" });
			if (!(e instanceof Refused)) console.error((e as Error).message);
		}
	})
	.listen(PORT);
console.log(`dorf keys on port ${PORT}`);
