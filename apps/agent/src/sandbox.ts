// The agent's only view of Agent Substrate. A Sandbox is one Actor made from the `sandbox`
// ActorTemplate; a request that reaches a paused Actor resumes it, so there is no resume call.
import { execFile, spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import net from "node:net";
import { Connection, RemoteExecutionEnv } from "@earendil-works/pi-env";

const ATESPACE = process.env.DORF_ATESPACE ?? "dorf";
const TEMPLATE = "sandbox";
const PIPE = new URL("./pipe.ts", import.meta.url).pathname;

export class SandboxNotFound extends Error {
	constructor(name: string) {
		super(`Sandbox ${name} not found; it was deleted`);
	}
}

function ate(name: string, ...args: string[]): Promise<string> {
	return new Promise((resolve, reject) => {
		execFile("kubectl", ["ate", ...args, "-a", ATESPACE, "-o", "json"], (error, stdout, stderr) => {
			if (!error) return resolve(stdout);
			const message = stderr.split("\n").filter((l) => l && !l.startsWith("Warning: no --token-file")).join(" ");
			reject(/not ?found/i.test(message) ? new SandboxNotFound(name) : new Error(`kubectl ate ${args.slice(0, 2).join(" ")}: ${message}`));
		});
	});
}

function freePort(): Promise<number> {
	return new Promise((resolve, reject) => {
		const server = net.createServer().listen(0, "127.0.0.1", () => {
			const { port } = server.address() as net.AddressInfo;
			server.close(() => resolve(port));
		});
		server.on("error", reject);
	});
}

// Wait for the local listener only; an HTTP probe would resume the Sandbox.
async function listening(port: number, proxy: ChildProcess): Promise<void> {
	for (let i = 0; i < 100 && proxy.exitCode === null; i++) {
		const up = await new Promise<boolean>((resolve) => {
			const socket = net.connect(port, "127.0.0.1", () => resolve(true)).on("error", () => resolve(false));
			socket.once("connect", () => socket.destroy());
		});
		if (up) return;
		await new Promise((r) => setTimeout(r, 100));
	}
	throw new Error(`proxy on port ${port} did not start`);
}

interface Open {
	connection: Connection;
	proxy?: { process: ChildProcess; port: number };
}

export class Sandboxes {
	#open = new Map<string, Open>();

	async create(): Promise<string> {
		const name = `s-${randomUUID().slice(0, 8)}`;
		await ate(name, "create", "actor", name, "--template", TEMPLATE);
		return name;
	}

	async state(name: string): Promise<string> {
		return JSON.parse(await ate(name, "get", "actor", name)).status.state;
	}

	// The proxy closes first, so a pi-env stream never lives through a pause; the connection
	// starts a new daemon on its next request.
	async pause(name: string): Promise<void> {
		this.#stopProxy(name);
		await ate(name, "suspend", "actor", name, "--mode=async");
	}

	async delete(name: string): Promise<void> {
		this.#open.get(name)?.connection.close();
		this.#stopProxy(name);
		this.#open.delete(name);
		await ate(name, "delete", "actor", name).catch((e) => {
			if (!(e instanceof SandboxNotFound)) throw e;
		});
	}

	env(name: string, cwd: string): RemoteExecutionEnv {
		return new RemoteExecutionEnv({ connection: this.#connection(name), id: `sandbox:${name}`, cwd });
	}

	#connection(name: string): Connection {
		let open = this.#open.get(name);
		if (!open) {
			open = {
				connection: new Connection({
					command: async () => ["node", PIPE, String(await this.#proxy(name))],
					onLog: (text) => process.stderr.write(`[pi-env ${name}] ${text}`),
				}),
			};
			this.#open.set(name, open);
		}
		return open.connection;
	}

	async #proxy(name: string): Promise<number> {
		const open = this.#open.get(name)!;
		if (open.proxy && open.proxy.process.exitCode === null) return open.proxy.port;
		// The Actor may be gone; `proxy` would still listen and fail per connection.
		await this.state(name);
		const port = await freePort();
		const child = spawn("kubectl", ["ate", "proxy", "actor", name, `${port}:80`, "-a", ATESPACE], { stdio: "ignore" });
		open.proxy = { process: child, port };
		await listening(port, child);
		return port;
	}

	#stopProxy(name: string): void {
		const open = this.#open.get(name);
		open?.proxy?.process.kill();
		if (open) open.proxy = undefined;
	}
}
