import http from "node:http";
import type { ConversationId } from "@earendil-works/pi-durable";
import type { Threads } from "./threads.ts";

type Reply = [status: number, body: unknown];

async function json(req: http.IncomingMessage): Promise<Record<string, unknown>> {
	let text = "";
	for await (const chunk of req) text += chunk;
	return text ? JSON.parse(text) : {};
}

async function route(threads: Threads, req: http.IncomingMessage): Promise<Reply> {
	const [, root, rawId, sub] = new URL(req.url ?? "/", "http://x").pathname.split("/");
	const id = Number(rawId) as ConversationId;
	if (root !== "threads") return [404, { error: "not found" }];
	const at = `${req.method} ${[rawId && ":id", sub].filter(Boolean).join("/")}`;
	switch (at) {
		case "GET ":
			return [200, await threads.list()];
		case "POST ": {
			const { prompt } = await json(req);
			if (typeof prompt !== "string" || !prompt) return [400, { error: "prompt required" }];
			return [201, await threads.create(prompt)];
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
		case "DELETE :id":
			return (await threads.delete(id)) ? [204, null] : [404, { error: "no such thread" }];
	}
	return [405, { error: "method not allowed" }];
}

export function serve(threads: Threads, port: number): http.Server {
	return http
		.createServer(async (req, res) => {
			let reply: Reply;
			try {
				reply = await route(threads, req);
			} catch (e) {
				reply = [500, { error: (e as Error).message }];
			}
			res.writeHead(reply[0], { "content-type": "application/json" }).end(reply[0] === 204 ? undefined : JSON.stringify(reply[1]));
		})
		.listen(port, "127.0.0.1");
}
