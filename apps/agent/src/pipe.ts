// pi-env transport: `node pipe.ts <atespace> <sandbox> serve --token <hex>`. Opens a
// `kubectl ate proxy` to the Sandbox, opens a WebSocket to its /pienv, and carries this
// process's stdio as binary messages. The proxy lives exactly as long as this process.
import { spawn } from "node:child_process";
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

const BUFFERED = 1 << 20;
const ws = new WebSocket(`ws://127.0.0.1:${port}/pienv`, { headers: { "X-Pienv-Args": JSON.stringify(args) } });
ws.binaryType = "arraybuffer";
let opened = false;
ws.onopen = () => {
	opened = true;
	process.stdin.on("data", (chunk: Buffer) => {
		ws.send(chunk);
		if (ws.bufferedAmount < BUFFERED) return;
		process.stdin.pause();
		const wait = setInterval(() => {
			if (ws.bufferedAmount >= BUFFERED / 2) return;
			clearInterval(wait);
			process.stdin.resume();
		}, 10);
	});
	process.stdin.on("end", () => ws.close());
};
ws.onmessage = (event) => process.stdout.write(new Uint8Array(event.data as ArrayBuffer));
ws.onclose = (event) => exit(opened ? 0 : 1, opened ? undefined : `websocket closed before open (${event.code} ${event.reason})`);
ws.onerror = () => exit(1, opened ? "websocket error" : "websocket failed to open");
