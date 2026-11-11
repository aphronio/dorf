// The gate's routes on this machine, for proofs that test the agent rather than Substrate: each Sandbox is a
// state, a folder its pi-env daemons start in, and those daemons. Proofs set the agent's workspace to /proc/self/cwd,
// which each daemon resolves to its own folder; other absolute paths and processes are shared. A pause closes the Sandbox's
// connections, a full set of slots answers 503, and a deleted Sandbox answers 404, as the real gate does.
import { type ChildProcess, spawn } from "node:child_process";
import { mkdirSync, rmSync } from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { join } from "node:path";
import { localDaemon } from "@earendil-works/pi-env";
import { WebSocketServer } from "ws";

type Sandbox = { state: "created" | "running" | "paused"; daemons: Set<ChildProcess> };

export async function localGate(slots: number, folders: string): Promise<string> {
	const sandboxes = new Map<string, Sandbox>();
	const running = () => [...sandboxes.values()].filter((s) => s.state === "running").length;

	// 404 for a Sandbox that is gone, 503 when no slot is free, as Substrate's resume answers.
	function resume(name: string): [number, unknown] | undefined {
		const sandbox = sandboxes.get(name);
		if (!sandbox) return [404, { error: `${name} not found` }];
		if (sandbox.state === "running") return;
		if (running() >= slots) return [503, { error: "no worker has room for the actor" }];
		sandbox.state = "running";
	}

	function close(sandbox: Sandbox): void {
		for (const daemon of sandbox.daemons) daemon.kill("SIGKILL");
		sandbox.daemons.clear();
	}

	function route(req: http.IncomingMessage): [number, unknown] {
		const url = new URL(req.url ?? "/", "http://x");
		const [, root, name, action] = url.pathname.split("/");
		if (root === "draining" && req.method === "GET") return [200, []];
		if (root !== "sandboxes" || !/^[a-z0-9-]+$/.test(name ?? "")) return [400, { error: "no such route" }];
		const sandbox = sandboxes.get(name);
		switch (`${req.method} ${action ?? ""}`) {
			case "POST ":
				if (sandbox) return [409, { error: `${name} exists` }];
				mkdirSync(join(folders, name), { recursive: true });
				sandboxes.set(name, { state: "created", daemons: new Set() });
				return [201, {}];
			case "GET ":
				return sandbox ? [200, { state: sandbox.state }] : [404, { error: `${name} not found` }];
			case "POST resume":
				return resume(name) ?? [204, null];
			case "POST suspend":
				if (!sandbox) return [404, { error: `${name} not found` }];
				close(sandbox);
				sandbox.state = "paused";
				return [204, null];
			case "DELETE ":
				if (!sandbox) return [404, { error: `${name} not found` }];
				close(sandbox);
				sandboxes.delete(name);
				rmSync(join(folders, name), { recursive: true, force: true });
				return [204, null];
		}
		return [405, { error: "method not allowed" }];
	}

	const server = http.createServer((req, res) => {
		const [status, body] = route(req);
		res.writeHead(status, { "content-type": "application/json" }).end(status === 204 ? undefined : JSON.stringify(body));
	});
	const sockets = new WebSocketServer({ noServer: true });
	server.on("upgrade", (req, socket, head) => {
		socket.on("error", () => socket.destroy());
		const name = req.url?.match(/^\/sandboxes\/([a-z0-9-]+)\/pienv$/)?.[1];
		if (!name) return socket.destroy();
		const refused = resume(name);
		if (refused) return socket.end(`HTTP/1.1 ${refused[0]} ${http.STATUS_CODES[refused[0]]}\r\n\r\n`);
		const sandbox = sandboxes.get(name)!;
		const args = JSON.parse(String(req.headers["x-pienv-args"] ?? "[]"));
		sockets.handleUpgrade(req, socket, head, (ws) => {
			const daemon = spawn(localDaemon(), args, { cwd: join(folders, name), stdio: ["pipe", "pipe", "inherit"] });
			sandbox.daemons.add(daemon);
			daemon.stdin.on("error", () => {});
			ws.on("message", (data: Buffer) => {
				if (daemon.stdin.write(data)) return;
				ws.pause();
				daemon.stdin.once("drain", () => ws.resume());
			});
			daemon.stdout.on("data", (chunk: Buffer) => {
				daemon.stdout.pause();
				ws.send(chunk, () => daemon.stdout.resume());
			});
			const end = () => {
				daemon.kill();
				ws.terminate();
			};
			ws.on("close", end).on("error", end);
			daemon.on("exit", () => {
				sandbox.daemons.delete(daemon);
				ws.close();
			});
		});
	});
	await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
	server.unref();
	return `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
}
