// The agent's only view of Agent Substrate. A Sandbox is one Actor made from the `sandbox`
// ActorTemplate; a request that reaches a paused Actor resumes it, so there is no resume call.
import { execFile } from "node:child_process";
import { randomUUID } from "node:crypto";
import { Connection, RemoteExecutionEnv } from "@earendil-works/pi-env";

const ATESPACE = "dorf";
const POOL_NAMESPACE = "dorf";
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
	return kubectl(name, ["ate", ...args, "-a", ATESPACE], stdin);
}

function kubectl(name: string, args: string[], stdin?: string): Promise<string> {
	return new Promise((resolve, reject) => {
		const child = execFile("kubectl", [...args, "-o", "json"], (error, stdout, stderr) => {
			if (!error) return resolve(stdout);
			const message = stderr.split("\n").filter((l) => l && !l.startsWith("Warning: no --token-file")).join(" ");
			reject(/not ?found/i.test(message) ? new SandboxNotFound(name) : new Error(`kubectl ${args.slice(0, 3).join(" ")}: ${message}`));
		});
		child.stdin?.end(stdin);
	});
}

type Actor = { metadata: { name: string }; status: { state: string; workerAssignment?: { workerPod: string } } };

export class Sandboxes {
	#connections = new Map<string, Connection>();
	#pausing = new Map<string, Promise<void>>();

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

	// Running Sandboxes on a worker that is going away.
	async evicted(): Promise<string[]> {
		const pods: { items: { metadata: { name: string; deletionTimestamp?: string } }[] } = JSON.parse(
			await kubectl("", ["get", "pods", "-n", POOL_NAMESPACE, "-l", "ate.dev/worker-pool"]),
		);
		const leaving = new Set(pods.items.filter((p) => p.metadata.deletionTimestamp).map((p) => p.metadata.name));
		if (!leaving.size) return [];
		const { actors = [] }: { actors?: Actor[] } = JSON.parse(await ate("", ["get", "actors"]));
		return actors
			.filter((a) => a.status.state === "ACTOR_STATE_RUNNING" && leaving.has(a.status.workerAssignment?.workerPod ?? ""))
			.map((a) => a.metadata.name);
	}

	// An idle pause closes the stream so the next call opens a fresh one at once; a drain keeps it
	// for the calls in flight to reattach, and waits until the snapshot survives losing the node.
	pause(name: string, reason: "idle" | "drain"): Promise<void> {
		let pausing = this.#pausing.get(name);
		if (!pausing) {
			if (reason === "idle") this.#close(name);
			pausing = ate(name, ["suspend", "actor", name, `--mode=${reason === "idle" ? "async" : "sync"}`])
				.then(() => {})
				.finally(() => this.#pausing.delete(name));
			this.#pausing.set(name, pausing);
		}
		return pausing;
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
					await this.#pausing.get(name)?.catch(() => {});
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
