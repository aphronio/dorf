import { readFileSync } from "node:fs";

const [url, tokenFile, ...args] = process.argv.slice(2);

function exit(code: number, message?: string): never {
	if (message) process.stderr.write(`pipe: ${message}\n`);
	process.exit(code);
}
process.on("SIGTERM", () => exit(0));

const BUFFERED = 1 << 20;
const ws = new WebSocket(url, {
	headers: { authorization: `Bearer ${readFileSync(tokenFile, "utf8").trim()}`, "X-Pienv-Args": JSON.stringify(args) },
});
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
