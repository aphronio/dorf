import { readFileSync } from "node:fs";
import { randomUUID } from "node:crypto";
import { Connection, RemoteExecutionEnv } from "@earendil-works/pi-env";

// Longer than the gate's own deadline, so the gate's error arrives first.
const GATE_TIMEOUT_MS = 6 * 60_000;
const PIPE = new URL("./pipe.ts", import.meta.url).pathname;
export const RETRY_ROOM_MS = 5_000;

export class SandboxNotFound extends Error {
	constructor(name: string) {
		super(`Sandbox ${name} not found; it was deleted`);
	}
}

class NoRoom extends Error {}
class Exists extends Error {}

export class Sandboxes {
	#gate: string;
	#tokenFile: string;
	// Pauses another Sandbox to free its worker memory; false when none can go.
	makeRoom: () => Promise<boolean> = async () => false;
	#connections = new Map<string, Connection>();
	#pausing = new Map<string, Promise<void>>();

	readonly #prefix: string;

	// Proofs name their Sandboxes apart, so a run can delete what a killed run left.
	constructor(gate: string, tokenFile: string, prefix = "s-") {
		this.#gate = gate;
		this.#tokenFile = tokenFile;
		this.#prefix = prefix;
	}

	async #call(name: string, method: string, path: string): Promise<any> {
		const token = readFileSync(this.#tokenFile, "utf8").trim();
		const res = await fetch(this.#gate + path, { method, headers: { authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(GATE_TIMEOUT_MS) });
		if (res.status === 404 && name) throw new SandboxNotFound(name);
		if (res.status === 503) throw new NoRoom(await res.text());
		if (res.status === 409) throw new Exists(await res.text());
		if (!res.ok) throw new Error(`gate ${method} ${path}: ${res.status} ${await res.text()}`);
		return res.status === 204 ? undefined : res.json();
	}

	newName(): string {
		return `${this.#prefix}${randomUUID().slice(0, 8)}`;
	}

	// One that exists already counts as created, so a start cut off by a restart can create it again.
	async create(name: string): Promise<void> {
		await this.#call("", "POST", `/sandboxes/${name}`).catch((e) => {
			if (!(e instanceof Exists)) throw e;
		});
	}

	// Starts the Sandbox, first pausing others while that makes room; false when no worker has room.
	async wake(name: string): Promise<boolean> {
		for (;;) {
			try {
				await this.#call(name, "POST", `/sandboxes/${name}/resume`);
				return true;
			} catch (e) {
				if (!(e instanceof NoRoom)) throw e;
			}
			if (!(await this.makeRoom())) return false;
		}
	}

	async #resume(name: string): Promise<void> {
		while (!(await this.wake(name))) await new Promise((r) => setTimeout(r, RETRY_ROOM_MS));
	}

	async state(name: string): Promise<string> {
		return (await this.#call(name, "GET", `/sandboxes/${name}`)).state;
	}

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
					await this.#resume(name);
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
