// Dorf's page on this machine with real data: the agent on the faux model, the proofs' stand-in gate, a few seeded
// Threads, and the page's dev server behind a proxy that adds the owner's header. Run `node apps/agent/test/page.ts`;
// Ctrl-C or a kill stops it all. Each run starts clean: a new data folder, a new gate, and no dev server left over.
import { type ChildProcess, spawn } from "node:child_process";
import { readFileSync, rmSync } from "node:fs";
import http from "node:http";
import { createServer, type AddressInfo } from "node:net";
import { AGENT, createThread, DATA, HEADERS, killAgent, runEnded, send, startAgent, until } from "./kit.ts";

const WEB = new URL("../../web/", import.meta.url).pathname;

// Next lets one dev server run per folder and names it in this lock; an earlier run's server would hold it.
try {
	process.kill(JSON.parse(readFileSync(`${WEB}.next/dev/lock`, "utf8")).pid, "SIGTERM");
	await new Promise((r) => setTimeout(r, 1000));
} catch {}

const freePort = () =>
	new Promise<number>((resolve) => {
		const probe = createServer().listen(0, "127.0.0.1", () => {
			const { port } = probe.address() as AddressInfo;
			probe.close(() => resolve(port));
		});
	});

// Only `tailscale serve` sets the owner's login in production; here this proxy does.
const proxy = http.createServer((req, res) => {
	const out = http.request(`${AGENT}${req.url}`, { method: req.method, headers: { ...req.headers, ...HEADERS, "content-type": req.headers["content-type"] ?? HEADERS["content-type"] } }, (answer) => {
		res.writeHead(answer.statusCode!, answer.headers);
		answer.pipe(res);
	});
	out.on("error", () => res.writeHead(502).end());
	req.pipe(out);
});
await new Promise<void>((resolve) => proxy.listen(0, "127.0.0.1", resolve));

// Five minutes idle before a Sandbox pauses, as in production.
await startAgent("faux", 5 * 60_000);

const port = Number(process.env.PORT) || (await freePort());
const next: ChildProcess = spawn(`${WEB}node_modules/.bin/next`, ["dev", "--hostname", "127.0.0.1", "--port", String(port)], {
	cwd: WEB,
	detached: true,
	stdio: ["ignore", "ignore", "inherit"],
	env: { ...process.env, DORF_AGENT: `http://127.0.0.1:${(proxy.address() as AddressInfo).port}`, DORF_DEV_ORIGINS: process.env.DORF_DEV_ORIGINS ?? "127.0.0.1,localhost" },
});

// However this process ends, the dev server and the agent go with it.
process.on("exit", () => {
	try {
		process.kill(-next.pid!, "SIGTERM");
	} catch {}
	killAgent("SIGKILL");
	rmSync(DATA, { recursive: true, force: true });
});
process.on("SIGINT", () => process.exit(0));
process.on("SIGTERM", () => process.exit(0));
next.on("exit", () => process.exit(1));

// Seeded Threads, so the page has a list, an answer with a tool call, a follow-up, and a failed run.
if (process.env.DORF_SEED !== "0") {
	await createThread("$ ls -la && git --version\n= Listed the workspace and **git**'s version.");
	const talk = await createThread("$ echo hello from a Sandbox");
	await runEnded(talk.id, "$ echo hello from a Sandbox");
	await send(talk.id, "$ printf '# Notes\\n\\n- one\\n- two\\n' > notes.md\n> notes.md");
	await createThread("$ echo before\n! the request was malformed");
}

const url = `http://127.0.0.1:${port}`;
await until("the page", async () => (await fetch(url).catch(() => undefined))?.ok, 120_000);
console.log(`\nDorf's page: ${url}  (Ctrl-C stops it)`);
