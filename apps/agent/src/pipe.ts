// pi-env transport: `node pipe.ts <atespace> <sandbox> serve --token <hex>`. Opens a
// `kubectl ate proxy` to the Sandbox, upgrades one request to its /pienv, and splices the
// stream onto this process's stdio. The proxy lives exactly as long as this process.
import { spawn } from "node:child_process";
import net from "node:net";
import { createInterface } from "node:readline";

const [atespace, sandbox, ...args] = process.argv.slice(2);
const proxy = spawn("kubectl", ["ate", "proxy", "actor", sandbox, "0:80", "-a", atespace], { stdio: ["ignore", "ignore", "pipe"] });

function exit(code: number, message?: string): never {
	if (message) process.stderr.write(`pipe: ${message}\n`);
	proxy.kill();
	process.exit(code);
}
process.on("SIGTERM", () => exit(0));
proxy.on("exit", () => exit(1, "proxy exited"));

const port = await new Promise<number>((resolve) => {
	createInterface({ input: proxy.stderr }).on("line", (line) => {
		const match = line.match(/^Proxying 127\.0\.0\.1:(\d+) /);
		if (match) resolve(Number(match[1]));
	});
});

// Substrate's router passes only websocket upgrades.
const socket = net.connect(port, "127.0.0.1", () =>
	socket.write(
		`GET /pienv HTTP/1.1\r\nHost: sandbox\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n` +
			`X-Pienv-Args: ${JSON.stringify(args)}\r\n\r\n`,
	),
);

let head = Buffer.alloc(0);
function onHead(chunk: Buffer): void {
	head = Buffer.concat([head, chunk]);
	const end = head.indexOf("\r\n\r\n");
	if (end < 0) return;
	socket.off("data", onHead);
	const status = head.subarray(0, head.indexOf("\r\n")).toString();
	if (!status.startsWith("HTTP/1.1 101")) exit(1, status);
	const rest = head.subarray(end + 4);
	if (rest.length) process.stdout.write(rest);
	socket.pipe(process.stdout);
	process.stdin.pipe(socket);
}

socket.on("data", onHead);
socket.on("error", (e) => exit(1, e.message));
socket.on("close", () => exit(0));
process.stdin.on("end", () => socket.end());
