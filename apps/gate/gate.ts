// Every call carries the caller's token to Substrate, which decides; the gate holds no rights over Sandboxes.
import { readFileSync } from "node:fs";
import http from "node:http";
import type { Duplex } from "node:stream";
import tls from "node:tls";
import { type Api, connect, httpStatus } from "./ateapi.ts";

const ATESPACE = "dorf";
const POOL_NAMESPACE = "dorf";
const NAME = /^[a-z0-9-]+$/;
const ROUTER_TIMEOUT_MS = 30_000;
const FAILED_PRECONDITION = 9;
const COMPLETED = "SNAPSHOT_STORAGE_STATE_COMPLETED";
const SERVICEDNS = "/run/ate/servicedns/trust-bundle.pem";
const PODIDENTITY = "/run/ate/podidentity/credential-bundle.pem";
const ROUTER = "atenet-router.ate-system.svc";
const KUBE = "/var/run/secrets/kubernetes.io/serviceaccount";
// With GitHub on, the egress gateway swaps the token from dorf-secrets/github into git's and gh's
// Authorization header, whatever placeholder they send.
const GITHUB = {
	https: {
		hostnames: ["github.com", "api.github.com", "uploads.github.com"],
		effects: { replaceHeaders: [{ header: "Authorization", prefix: "Basic ", credentialUri: "ate-secret://k8s.io/default/dorf-secrets/github/basic" }] },
	},
};
const EGRESS = [
	{ tlsPassthrough: { hostnames: ["*"], ports: { all: {} } } },
	{ http: { hostnames: ["*"] } },
	...(process.env.DORF_GITHUB ? [GITHUB] : []),
];

const api: Api = connect("api.ate-system.svc:443", readFileSync(SERVICEDNS), "api.ate-system.svc");
const ref = (name: string) => ({ atespace: ATESPACE, name });

class HttpError extends Error {
	status: number;
	constructor(status: number, message: string) {
		super(message);
		this.status = status;
	}
}

// `refused` answers a FailedPrecondition with its own status instead of 409.
async function call(method: string, request: object, token: string, refused?: number): Promise<any> {
	return api(method, request, token).catch((e) => {
		throw new HttpError(refused && e.code === FAILED_PRECONDITION ? refused : httpStatus(e), `${method}: ${e.details ?? e.message}`);
	});
}

async function listAll(method: string, key: string, token: string): Promise<any[]> {
	const items: any[] = [];
	let pageToken = "";
	do {
		const page = await call(method, { atespace: ATESPACE, options: { pageToken } }, token);
		items.push(...(page[key] ?? []));
		pageToken = page.nextPageToken ?? "";
	} while (pageToken);
	return items;
}

function bearer(req: http.IncomingMessage): string {
	const token = req.headers.authorization?.match(/^Bearer (\S+)$/)?.[1];
	if (!token) throw new HttpError(401, "bearer token required");
	return token;
}

function currentPodCertificate(): { key?: string; cert?: string } {
	const bundle = readFileSync(PODIDENTITY, "utf8");
	return {
		key: bundle.match(/-----BEGIN [A-Z ]*PRIVATE KEY-----[^-]+-----END [A-Z ]*PRIVATE KEY-----/)?.[0],
		cert: bundle.match(/-----BEGIN CERTIFICATE-----[^-]+-----END CERTIFICATE-----/g)?.join("\n"),
	};
}

function routerTunnel(name: string): Promise<tls.TLSSocket> {
	return new Promise((resolve, reject) => {
		const socket = tls.connect({ host: ROUTER, port: 8444, servername: ROUTER, ca: readFileSync(SERVICEDNS), ...currentPodCertificate() }, () => {
			socket.write(`CONNECT ${name}:80 HTTP/1.1\r\nHost: ${name}:80\r\nate-target-actor: ${ATESPACE}/${name}\r\n\r\n`);
		});
		socket.setTimeout(ROUTER_TIMEOUT_MS, () => socket.destroy(new HttpError(504, "router did not answer")));
		let head = Buffer.alloc(0);
		const onData = (chunk: Buffer) => {
			head = Buffer.concat([head, chunk]);
			const end = head.indexOf("\r\n\r\n");
			if (end < 0) return;
			socket.off("data", onData);
			const status = head.subarray(0, head.indexOf("\r\n")).toString();
			if (!/^HTTP\/1\.[01] 200/.test(status)) return reject(new HttpError(502, `router: ${status}`));
			socket.setTimeout(0);
			if (head.length > end + 4) socket.unshift(head.subarray(end + 4));
			resolve(socket);
		};
		socket.on("data", onData).once("error", reject).once("close", () => reject(new HttpError(502, "router closed the tunnel")));
	});
}

async function forwardUpgradeToSandbox(req: http.IncomingMessage, client: Duplex, head: Buffer, name: string) {
	await call("ResumeActor", { actor: ref(name) }, bearer(req));
	const tunnel = await routerTunnel(name);
	const headers = Object.entries(req.headers).filter(([k]) => k !== "authorization" && k !== "host");
	tunnel.write(`GET /pienv HTTP/1.1\r\nHost: ${name}\r\n${headers.map(([k, v]) => `${k}: ${v}\r\n`).join("")}\r\n`);
	tunnel.write(head);
	tunnel.pipe(client).pipe(tunnel);
	const end = () => {
		tunnel.destroy();
		client.destroy();
	};
	tunnel.on("error", end).on("close", end);
	client.on("error", end).on("close", end);
}

function sandboxState(actorState: string): string {
	return actorState === "ACTOR_STATE_SUSPENDED" ? "paused" : actorState.replace(/^ACTOR_STATE_/, "").toLowerCase();
}

async function newestTemplate(token: string): Promise<string> {
	const { actorTemplates = [] } = await call("ListActorTemplates", { atespace: ATESPACE }, token);
	const newest = actorTemplates
		.filter((t: any) => t.metadata.name.startsWith("sandbox-"))
		.sort((a: any, b: any) => Number(b.metadata.createTime.seconds) - Number(a.metadata.createTime.seconds))[0];
	if (!newest) throw new HttpError(409, "no Sandbox template; run deploy/up.sh");
	return newest.metadata.name;
}

async function leavingPods(): Promise<Set<string>> {
	const res = await fetch(`https://kubernetes.default.svc/api/v1/namespaces/${POOL_NAMESPACE}/pods?labelSelector=ate.dev/worker-pool`, {
		headers: { authorization: `Bearer ${readFileSync(`${KUBE}/token`, "utf8")}` },
	});
	if (!res.ok) throw new HttpError(502, `pods: ${res.status}`);
	const { items } = (await res.json()) as { items: { metadata: { name: string; deletionTimestamp?: string } }[] };
	return new Set(items.filter((p) => p.metadata.deletionTimestamp).map((p) => p.metadata.name));
}

// A snapshot is borrowed while a Sandbox made from it has not yet saved its own: that Sandbox's newest finished
// snapshot is still the tag's. Deleting the tag then would lose the Sandbox.
async function snapshots(token: string): Promise<{ name: string; ready: boolean; borrowed: boolean }[]> {
	const [tags, actors] = await Promise.all([listAll("ListTags", "tags", token), listAll("ListActors", "actors", token)]);
	const borrowed = new Set<string>();
	for (const actor of actors) {
		const newest = (actor.status?.snapshots ?? [])
			.filter((s: any) => s.durableSnapshot?.state === COMPLETED)
			.sort((a: any, b: any) => b.generation - a.generation)[0];
		const uri = newest?.durableSnapshot.object?.snapshotUri;
		if (newest?.owner === "SNAPSHOT_OWNER_TAG" && uri) borrowed.add(uri);
	}
	return tags.map((tag: any) => {
		const durable = tag.status?.snapshot?.durableSnapshot;
		const uri = durable?.object?.snapshotUri;
		return { name: tag.metadata.name, ready: durable?.state === COMPLETED && !tag.metadata.deleteTime, borrowed: Boolean(uri) && borrowed.has(uri) };
	});
}

async function route(req: http.IncomingMessage): Promise<[number, unknown]> {
	const token = bearer(req);
	const [, root, name, action] = new URL(req.url ?? "/", "http://x").pathname.split("/");
	if (root === "draining" && req.method === "GET") {
		const { actors = [] } = await call("ListActors", { atespace: ATESPACE }, token);
		const leaving = await leavingPods();
		return [200, actors.filter((a: any) => a.status.state === "ACTOR_STATE_RUNNING" && leaving.has(a.status.workerAssignment?.workerPod)).map((a: any) => a.metadata.name)];
	}
	const from = new URL(req.url ?? "/", "http://x").searchParams.get("from");
	if (from !== null && !NAME.test(from)) return [400, { error: "from must name a Sandbox or snapshot" }];
	if (root === "snapshots" && req.method === "GET" && name === undefined) return [200, await snapshots(token)];
	if (root === "snapshots" && NAME.test(name ?? "") && action === undefined) {
		if (req.method === "POST" && from) {
			await call("CreateTag", { tag: { metadata: ref(name), scope: "TAG_SCOPE_ATESPACE", sourceActor: ref(from) } }, token);
			return [201, {}];
		}
		if (req.method === "DELETE") {
			await call("DeleteTag", { tag: ref(name) }, token);
			return [204, null];
		}
		return [405, { error: "method not allowed" }];
	}
	// 400, not 404: the agent reads 404 as a Sandbox that no longer exists.
	if (root !== "sandboxes" || !NAME.test(name ?? "")) return [400, { error: "no such route" }];
	switch (`${req.method} ${action ?? ""}`) {
		// 412: Substrate refused the snapshot, as one taken under another template or being deleted.
		case "POST ": {
			const actorTemplate = ref(await newestTemplate(token));
			await call("CreateActor", { actor: { metadata: ref(name), actorTemplate, ...(from ? { sourceTag: ref(from) } : {}) } }, token, 412);
			await call("CreateActorEgressPolicy", { actor: ref(name), egressPolicy: { metadata: ref("default"), rules: EGRESS } }, token);
			return [201, {}];
		}
		case "GET ":
			return [200, { state: sandboxState((await call("GetActor", { actor: ref(name) }, token)).status.state) }];
		// 503: no worker has room until a Sandbox pauses.
		case "POST resume":
			await call("ResumeActor", { actor: ref(name) }, token);
			return [204, null];
		case "POST suspend": {
			const mode = new URL(req.url ?? "/", "http://x").searchParams.get("mode") === "sync" ? "DURABILITY_MODE_SYNCHRONOUS" : "DURABILITY_MODE_ASYNCHRONOUS";
			await call("SuspendActor", { actor: ref(name), options: { mode } }, token);
			return [204, null];
		}
		case "DELETE ":
			await call("DeleteActor", { actor: ref(name) }, token);
			return [204, null];
	}
	return [405, { error: "method not allowed" }];
}

const server = http.createServer(async (req, res) => {
	let reply: [number, unknown];
	try {
		reply = await route(req);
	} catch (e) {
		reply = [e instanceof HttpError ? e.status : 500, { error: (e as Error).message }];
	}
	res.writeHead(reply[0], { "content-type": "application/json" }).end(reply[0] === 204 ? undefined : JSON.stringify(reply[1]));
});

server.on("upgrade", (req, socket, head) => {
	// Without a listener, a client resetting the connection mid-check would crash the gate.
	socket.on("error", () => socket.destroy());
	const name = req.url?.match(/^\/sandboxes\/([a-z0-9-]+)\/pienv$/)?.[1];
	if (!name) return socket.destroy();
	forwardUpgradeToSandbox(req, socket, head, name).catch((e) => {
		const status = e instanceof HttpError ? e.status : 502;
		socket.end(`HTTP/1.1 ${status} ${http.STATUS_CODES[status]}\r\ncontent-type: text/plain\r\n\r\n${e.message}\n`);
	});
});

// As PID 1, Node ignores SIGTERM unless handled; open tunnels end with the process either way.
process.on("SIGTERM", () => process.exit(0));
server.listen(8080);
