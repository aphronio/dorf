// pi-env transport: `node pipe.ts <port> serve --token <hex>`. Upgrades one HTTP request to the
// Sandbox's /pienv through a local proxy port and splices the stream onto this process's stdio.
// Substrate's router carries only HTTP; the Sandbox init runs the daemon behind the upgrade.
import net from "node:net";

const [port, ...args] = process.argv.slice(2);
const socket = net.connect(Number(port), "127.0.0.1", () =>
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
	if (!status.startsWith("HTTP/1.1 101")) {
		process.stderr.write(`pipe: ${status}\n`);
		process.exit(1);
	}
	const rest = head.subarray(end + 4);
	if (rest.length) process.stdout.write(rest);
	socket.pipe(process.stdout);
	process.stdin.pipe(socket);
}

socket.on("data", onHead);
socket.on("error", (e) => {
	process.stderr.write(`pipe: ${e.message}\n`);
	process.exit(1);
});
socket.on("close", () => process.exit(0));
process.stdin.on("end", () => socket.end());
