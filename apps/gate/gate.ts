// Dorf's door into the cluster and the only code that knows Agent Substrate. A Sandbox is one Actor
// in atespace dorf, made from the newest `sandbox-*` ActorTemplate. Every call carries the caller's
// token to Substrate, which decides; the gate holds no rights over Sandboxes of its own.
import { readFileSync } from "node:fs";
import http from "node:http";
import type { Duplex } from "node:stream";
import tls from "node:tls";
import { type Api, connect, httpStatus } from "./ateapi.ts";

const ATESPACE = "dorf";
const POOL_NAMESPACE = "dorf";
const NAME = /^[a-z0-9-]+$/;
const ROUTER_TIMEOUT_MS = 30_000;
const SERVICEDNS = "/run/ate/servicedns/trust-bundle.pem";
const PODIDENTITY = "/run/ate/podidentity/credential-bundle.pem";
const ROUTER = "atenet-router.ate-system.svc";
const KUBE = "/var/run/secrets/kubernetes.io/serviceaccount";
// Like a machine of your own: every host, TLS passed through untouched. GitHub is intercepted so
// the egress gateway can put the token from Secret dorf-secrets/github into git's Basic auth.
const EGRESS = [
	{ tlsPassthrough: { hostnames: ["*"], ports: { all: {} } } },
	{ http: { hostnames: ["*"] } },
	{
		https: {
			hostnames: ["github.com"],
			effects: { replaceHeaders: [{ header: "Authorization", prefix: "Basic ", credentialUri: "ate-secret://k8s.io/default/dorf-secrets/github/basic" }] },
		},
	},
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

async function call(method: string, request: object, token: string): Promise<any> {
	return api(method, request, token).catch((e) => {
		throw new HttpError(httpStatus(e), `${method}: ${e.details ?? e.message}`);
	});
}

function bearer(req: http.IncomingMessage): string {
	const token = req.headers.authorization?.match(/^Bearer (\S+)$/)?.[1];
	if (!token) throw new HttpError(401, "bearer token required");
	return token;
}

// The router authenticates the gate by its pod certificate, which the kubelet rotates in place.
function routerTunnel(name: string): Promise<tls.TLSSocket> {
	const bundle = readFileSync(PODIDENTITY, "utf8");
	const key = bundle.match(/-----BEGIN [A-Z ]*PRIVATE KEY-----[^-]+-----END [A-Z ]*PRIVATE KEY-----/)?.[0];
	const cert = bundle.match(/-----BEGIN CERTIFICATE-----[^-]+-----END CERTIFICATE-----/g)?.join("\n");
	return new Promise((resolve, reject) => {
		const socket = tls.connect({ host: ROUTER, port: 8444, servername: ROUTER, ca: readFileSync(SERVICEDNS), key, cert }, () => {
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

// The Sandbox's init answers the WebSocket itself; the gate forwards the upgrade and then bytes.
async function pienv(req: http.IncomingMessage, client: Duplex, head: Buffer, name: string) {
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

// A Sandbox's state in Dorf's words: running, paused, crashed, and so on.
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

// Worker pods being deleted, read with the gate's own ServiceAccount, which may list only the pool's pods.
async function leavingPods(): Promise<Set<string>> {
	const res = await fetch(`https://kubernetes.default.svc/api/v1/namespaces/${POOL_NAMESPACE}/pods?labelSelector=ate.dev/worker-pool`, {
		headers: { authorization: `Bearer ${readFileSync(`${KUBE}/token`, "utf8")}` },
	});
	if (!res.ok) throw new HttpError(502, `pods: ${res.status}`);
	const { items } = (await res.json()) as { items: { metadata: { name: string; deletionTimestamp?: string } }[] };
	return new Set(items.filter((p) => p.metadata.deletionTimestamp).map((p) => p.metadata.name));
}

async function route(req: http.IncomingMessage): Promise<[number, unknown]> {
	const token = bearer(req);
	const [, root, name, action] = new URL(req.url ?? "/", "http://x").pathname.split("/");
	if (root === "draining" && req.method === "GET") {
		const { actors = [] } = await call("ListActors", { atespace: ATESPACE }, token);
		const leaving = await leavingPods();
		return [200, actors.filter((a: any) => a.status.state === "ACTOR_STATE_RUNNING" && leaving.has(a.status.workerAssignment?.workerPod)).map((a: any) => a.metadata.name)];
	}
	// 400, not 404: the agent reads 404 as a Sandbox that no longer exists.
	if (root !== "sandboxes" || !NAME.test(name ?? "")) return [400, { error: "no such route" }];
	switch (`${req.method} ${action ?? ""}`) {
		case "POST ": {
			const actorTemplate = ref(await newestTemplate(token));
			await call("CreateActor", { actor: { metadata: ref(name), actorTemplate } }, token);
			await call("CreateActorEgressPolicy", { actor: ref(name), egressPolicy: { metadata: ref("default"), rules: EGRESS } }, token);
			return [201, {}];
		}
		case "GET ":
			return [200, { state: sandboxState((await call("GetActor", { actor: ref(name) }, token)).status.state) }];
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
	pienv(req, socket, head, name).catch((e) => {
		const status = e instanceof HttpError ? e.status : 502;
		socket.end(`HTTP/1.1 ${status} ${http.STATUS_CODES[status]}\r\ncontent-type: text/plain\r\n\r\n${e.message}\n`);
	});
});

// As PID 1, Node ignores SIGTERM unless handled; open tunnels end with the process either way.
process.on("SIGTERM", () => process.exit(0));
server.listen(8080);
