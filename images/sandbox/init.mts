// The Sandbox's main process. Substrate's router carries only HTTP, so one upgraded request
// becomes the stdio of a pi-env daemon.
import { spawn } from "node:child_process";
import http from "node:http";

// Substrate signals the main process when its worker goes away; a process that exits cannot be paused.
process.on("SIGTERM", () => console.log("SIGTERM: staying up to be paused"));

const server = http.createServer((req, res) => {
	res.writeHead(req.url === "/readyz" ? 200 : 404).end();
});

server.on("upgrade", (req, socket, head) => {
	if (req.url !== "/pienv") return socket.destroy();
	let args: string[];
	try {
		args = JSON.parse(String(req.headers["x-pienv-args"] ?? "[]"));
	} catch {
		return socket.destroy();
	}
	const daemon = spawn("/usr/local/bin/pi-env", args, { stdio: ["pipe", "pipe", "inherit"] });
	socket.write("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n");
	if (head.length) daemon.stdin.write(head);
	socket.pipe(daemon.stdin);
	daemon.stdout.pipe(socket);
	const end = () => {
		daemon.kill();
		socket.destroy();
	};
	socket.on("close", end).on("error", end);
	daemon.on("exit", () => socket.end());
});

server.listen(80);
