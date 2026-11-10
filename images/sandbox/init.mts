// The Sandbox's main process. Substrate's router carries only HTTP and WebSocket, so each
// WebSocket on /pienv becomes the stdio of a fresh pi-env daemon: one binary message per chunk.
import { spawn } from "node:child_process";
import http from "node:http";
import { WebSocketServer } from "ws";

// Substrate signals the main process when its worker goes away; a process that exits cannot be paused.
process.on("SIGTERM", () => console.log("SIGTERM: staying up to be paused"));

const server = http.createServer((req, res) => {
	res.writeHead(req.url === "/readyz" ? 200 : 404).end();
});
const sockets = new WebSocketServer({ noServer: true });

server.on("upgrade", (req, socket, head) => {
	if (req.url !== "/pienv") return socket.destroy();
	let args: string[];
	try {
		args = JSON.parse(String(req.headers["x-pienv-args"] ?? "[]"));
	} catch {
		return socket.destroy();
	}
	sockets.handleUpgrade(req, socket, head, (ws) => {
		const daemon = spawn("/usr/local/bin/pi-env", args, { stdio: ["pipe", "pipe", "inherit"] });
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
		daemon.on("exit", () => ws.close());
	});
});

server.listen(80);
