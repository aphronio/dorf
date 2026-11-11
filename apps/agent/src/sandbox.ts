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

// Substrate would not make a Sandbox from the snapshot: it was taken under an older template, or is being deleted.
export class SnapshotRefused extends Error {}

export type Snapshot = { name: string; ready: boolean; borrowed: boolean };

class NoRoom extends Error {}
class Exists extends Error {}
class Gone extends Error {}

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
		if (res.status === 404) throw name ? new SandboxNotFound(name) : new Gone(await res.text());
		if (res.status === 503) throw new NoRoom(await res.text());
		if (res.status === 409) throw new Exists(await res.text());
		if (res.status === 412) throw new SnapshotRefused(await res.text());
		if (!res.ok) throw new Error(`gate ${method} ${path}: ${res.status} ${await res.text()}`);
		return res.status === 204 ? undefined : res.json();
	}

	newName(): string {
		return `${this.#prefix}${randomUUID().slice(0, 8)}`;
	}

	// A name of this agent's own, as for a snapshot or a Sandbox that is not a Thread's.
	named(suffix: string): string {
		return `${this.#prefix}${suffix}`;
	}

	// One that exists already counts as created, so a start cut off by a restart can create it again. `from` names a
	// snapshot to start from; Substrate's refusal throws SnapshotRefused.
	async create(name: string, from?: string): Promise<void> {
		await this.#call("", "POST", `/sandboxes/${name}${from ? `?from=${from}` : ""}`).catch((e) => {
			if (!(e instanceof Exists)) throw e;
		});
	}

	// Starts the Sandbox, first pausing others while that makes room unless `makeRoom` is false; false when no worker
	// has room.
	async wake(name: string, makeRoom = true): Promise<boolean> {
		for (;;) {
			try {
				await this.#call(name, "POST", `/sandboxes/${name}/resume`);
				return true;
			} catch (e) {
				if (!(e instanceof NoRoom)) throw e;
			}
			if (!makeRoom || !(await this.makeRoom())) return false;
		}
	}

	async #resume(name: string, makeRoom: boolean): Promise<void> {
		while (!(await this.wake(name, makeRoom))) await new Promise((r) => setTimeout(r, RETRY_ROOM_MS));
	}

	// Saves a paused Sandbox's disk and memory under `name`; returns once the snapshot is ready.
	async snapshot(name: string, from: string): Promise<void> {
		await this.#call("", "POST", `/snapshots/${name}?from=${from}`);
	}

	snapshots(): Promise<Snapshot[]> {
		return this.#call("", "GET", "/snapshots");
	}

	async deleteSnapshot(name: string): Promise<void> {
		await this.#call("", "DELETE", `/snapshots/${name}`).catch((e) => {
			if (!(e instanceof Gone)) throw e;
		});
	}

	async state(name: string): Promise<string> {
		return (await this.#call(name, "GET", `/sandboxes/${name}`)).state;
	}

	evicted(): Promise<string[]> {
		return this.#call("", "GET", "/draining");
	}

	// An idle pause closes the stream so the next call opens a fresh one at once; a drain keeps it
	// for the calls in flight to reattach, and waits until the snapshot survives losing the node. A pause to save
	// the Sandbox as a snapshot does both: no stream is left in the memory it saves.
	pause(name: string, reason: "idle" | "drain" | "save"): Promise<void> {
		let pausing = this.#pausing.get(name);
		if (!pausing) {
			if (reason !== "drain") this.#close(name);
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

	// `makeRoom` false never pauses another Sandbox to resume this one: it waits for room instead.
	env(name: string, cwd: string, makeRoom = true): RemoteExecutionEnv {
		let connection = this.#connections.get(name);
		if (!connection) {
			connection = new Connection({
				command: async () => {
					await this.#pausing.get(name)?.catch(() => {});
					await this.#resume(name, makeRoom);
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
