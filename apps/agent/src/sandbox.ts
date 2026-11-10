// The agent's only view of Agent Substrate: Dorf's gate (apps/gate), called with a token Substrate
// scopes to Dorf's Sandboxes. A request that reaches a paused Sandbox resumes it, so there is no
// resume call.
import { readFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { Connection, RemoteExecutionEnv } from "@earendil-works/pi-env";

const PIPE = new URL("./pipe.ts", import.meta.url).pathname;

export class SandboxNotFound extends Error {
	constructor(name: string) {
		super(`Sandbox ${name} not found; it was deleted`);
	}
}

export class Sandboxes {
	#gate: string;
	// Read on every call, so a token rotated on disk is picked up.
	#tokenFile: string;
	#connections = new Map<string, Connection>();
	#pausing = new Map<string, Promise<void>>();

	constructor(gate: string, tokenFile: string) {
		this.#gate = gate;
		this.#tokenFile = tokenFile;
	}

	async #call(name: string, method: string, path: string): Promise<any> {
		const token = readFileSync(this.#tokenFile, "utf8").trim();
		const res = await fetch(this.#gate + path, { method, headers: { authorization: `Bearer ${token}` } });
		if (res.status === 404 && name) throw new SandboxNotFound(name);
		if (!res.ok) throw new Error(`gate ${method} ${path}: ${res.status} ${await res.text()}`);
		return res.status === 204 ? undefined : res.json();
	}

	newName(): string {
		return `s-${randomUUID().slice(0, 8)}`;
	}

	async create(name: string): Promise<void> {
		await this.#call("", "POST", `/sandboxes/${name}`);
	}

	async state(name: string): Promise<string> {
		return (await this.#call(name, "GET", `/sandboxes/${name}`)).state;
	}

	// Running Sandboxes on a node that is going away.
	evicted(): Promise<string[]> {
		return this.#call("", "GET", "/draining");
	}

	// An idle pause closes the stream so the next call opens a fresh one at once; a drain keeps it
	// for the calls in flight to reattach, and waits until the snapshot survives losing the node.
	pause(name: string, reason: "idle" | "drain"): Promise<void> {
		let pausing = this.#pausing.get(name);
		if (!pausing) {
			if (reason === "idle") this.#close(name);
			pausing = this.#call(name, "POST", `/sandboxes/${name}/suspend?mode=${reason === "idle" ? "async" : "sync"}`)
				.then(() => {})
				.finally(() => this.#pausing.delete(name));
			this.#pausing.set(name, pausing);
		}
		return pausing;
	}

	async delete(name: string): Promise<void> {
		this.#close(name);
		await this.#call(name, "DELETE", `/sandboxes/${name}`).catch((e) => {
			if (!(e instanceof SandboxNotFound)) throw e;
		});
	}

	env(name: string, cwd: string): RemoteExecutionEnv {
		let connection = this.#connections.get(name);
		if (!connection) {
			connection = new Connection({
				command: async () => {
					await this.#pausing.get(name)?.catch(() => {});
					await this.state(name);
					return ["node", PIPE, `${this.#gate.replace(/^http/, "ws")}/sandboxes/${name}/pienv`, this.#tokenFile];
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
