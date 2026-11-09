// The agent's only view of Agent Substrate. A Sandbox is one Actor made from the `sandbox`
// ActorTemplate; a request that reaches a paused Actor resumes it, so there is no resume call.
import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { Connection, RemoteExecutionEnv } from "@earendil-works/pi-env";

const ATESPACE = "dorf";
const TEMPLATE = "sandbox";
const PIPE = new URL("./pipe.ts", import.meta.url).pathname;
// Like a machine of your own: every host, TLS passed through untouched. Substrate denies all by default.
const OPEN_EGRESS = 'rules: [{tlsPassthrough: {hostnames: ["*"], ports: {all: {}}}}, {http: {hostnames: ["*"]}}]';

export class SandboxNotFound extends Error {
	constructor(name: string) {
		super(`Sandbox ${name} not found; it was deleted`);
	}
}

function ate(name: string, args: string[], stdin?: string): Promise<string> {
	return new Promise((resolve, reject) => {
		const child = execFile("kubectl", ["ate", ...args, "-a", ATESPACE, "-o", "json"], (error, stdout, stderr) => {
			if (!error) return resolve(stdout);
			const message = stderr.split("\n").filter((l) => l && !l.startsWith("Warning: no --token-file")).join(" ");
			reject(/not ?found/i.test(message) ? new SandboxNotFound(name) : new Error(`kubectl ate ${args.slice(0, 2).join(" ")}: ${message}`));
		});
		child.stdin?.end(stdin);
	});
}

export class Sandboxes {
	#connections = new Map<string, Connection>();

	newName(): string {
		return `s-${randomUUID().slice(0, 8)}`;
	}

	async create(name: string): Promise<void> {
		await ate(name, ["create", "actor", name, "--template", TEMPLATE]);
		await ate(name, ["create", "egress-policy", name, "-f", "-"], OPEN_EGRESS);
	}

	async state(name: string): Promise<string> {
		return JSON.parse(await ate(name, ["get", "actor", name])).status.state;
	}

	// The stream closes first: one that lives through a pause fails its next request.
	async pause(name: string): Promise<void> {
		this.#close(name);
		await ate(name, ["suspend", "actor", name, "--mode=async"]);
	}

	async delete(name: string): Promise<void> {
		this.#close(name);
		await ate(name, ["delete", "actor", name]).catch((e) => {
			if (!(e instanceof SandboxNotFound)) throw e;
		});
	}

	env(name: string, cwd: string): RemoteExecutionEnv {
		let connection = this.#connections.get(name);
		if (!connection) {
			connection = new Connection({
				command: async () => {
					await this.state(name);
					return ["node", PIPE, ATESPACE, name];
				},
				onLog: (text) => process.stderr.write(`[pi-env ${name}] ${text}`),
			});
			this.#connections.set(name, connection);
		}
		return new RemoteExecutionEnv({ connection, id: `sandbox:${name}`, cwd });
	}

	#close(name: string): void {
		this.#connections.get(name)?.close();
		this.#connections.delete(name);
	}
}
