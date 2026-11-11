import { execFileSync } from "node:child_process";
import { chmodSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { call, createThread, DATA, fail, follow, killAgent, log, pass, runEnded, startAgent, until } from "./kit.ts";

// Repositories on this machine that the stand-ins' git clones in place of github.com's, each with this `.agents/setup`.
const REPOS = join(DATA, "repos");
function repository(name: string, setup: string): string {
	const dir = join(REPOS, "work", name);
	mkdirSync(join(dir, ".agents"), { recursive: true });
	writeFileSync(join(dir, ".agents", "setup"), `#!/bin/sh\n${setup}\n`);
	chmodSync(join(dir, ".agents", "setup"), 0o755);
	const git = (...args: string[]) => execFileSync("git", ["-c", "user.name=proof", "-c", "user.email=proof@example.com", ...args], { cwd: dir, stdio: "ignore" });
	git("init", "-q");
	git("add", ".");
	git("commit", "-qm", "setup");
	git("clone", "-q", "--bare", dir, join(REPOS, "proof", `${name}.git`));
	return `proof/${name}`;
}
Object.assign(process.env, { GIT_CONFIG_COUNT: "1", GIT_CONFIG_KEY_0: `url.${REPOS}/.insteadOf`, GIT_CONFIG_VALUE_0: "https://github.com/" });

type Listed = { id: number; title: string; starting?: true; waiting?: { since: string; busy: number[] }; working?: true };

// Long enough that no Sandbox pauses on its own during the proof.
await startAgent("faux", 60 * 60_000);
let list = follow("/threads/events");
const listed = () => (list.events.at(-1)?.threads ?? []) as Listed[];
const state = (id: number) => listed().find((t) => t.id === id);

// Every slot of the stand-in's worker holds a Thread at work, so none can be paused to make room.
const busy = "$ sleep 25";
const holders: { id: number; sandbox: string }[] = [];
for (let i = 0; i < 3; i++) holders.push(await createThread(busy));
await until("every slot at work", async () => holders.every((h) => state(h.id)?.working));
log("1. three Threads work in the worker's three slots");

const waited = "$ echo waited";
const create = () => call("POST", "/threads", { prompt: waited, request: "draft-1" });
const began = Date.now();
const first = await create();
const answered = Date.now() - began;
if (first.status !== 201 || answered > 2_000) await fail(`create: ${first.status} after ${answered} ms`);
const { id } = first.json;
await until("the new Thread to wait for a machine", async () => state(id)?.starting && state(id)?.waiting);
const holding = holders.map((h) => h.id).join();
await until("the waiting Thread to name the Threads holding the machines", async () => state(id)?.waiting?.busy.join() === holding);
const since = state(id)!.waiting!.since;
log("2. POST /threads answered at once; the Thread waits for a machine and names the Threads holding them", `${answered} ms, ${JSON.stringify(state(id))}`);

const again = await create();
if (again.status !== 201 || again.json.id !== id || again.json.sandbox !== first.json.sandbox) await fail(`repeat: ${JSON.stringify(again)}`);
if (listed().filter((t) => t.title === waited).length !== 1) await fail(`the repeat made another Thread: ${JSON.stringify(listed())}`);
log("3. a repeat with the same request id returns the same Thread", `Thread ${again.json.id}`);

list.close();
killAgent("SIGKILL");
await startAgent("faux", 60 * 60_000);
list = follow("/threads/events");
await until("the list after the restart", async () => state(id));
if (!state(id)?.starting || state(id)?.waiting?.since !== since || (await call("GET", `/threads/${id}`)).json.entries.length) await fail(`after the restart: ${JSON.stringify(state(id))}`);
const refused = await call("POST", `/threads/${id}/messages`, { text: "$ echo too soon" });
if (refused.status !== 409) await fail(`a message before the first got ${refused.status}`);
log("4. after a restart it still waits, counted from the first wait, and refuses a second message until its first is in");

const [result] = await runEnded(id, waited, 90_000);
const seconds = (Date.now() - began) / 1000;
if (result?.output.trim() !== "waited" || state(id)?.starting || state(id)?.waiting) await fail(`once room freed: ${JSON.stringify(result)} ${JSON.stringify(state(id))}`);
if (seconds < 15) await fail(`started after ${seconds} s, before a slot was free`);
log("5. once a run ended and its Sandbox paused, the Thread started and answered", `${seconds.toFixed(1)} s after the POST`);
list.close();

for (const t of [...holders, { id }]) if ((await call("DELETE", `/threads/${t.id}`)).status !== 204) await fail("delete");
log("6. threads deleted");

const ready = await createThread("$ cat installed", repository("ready", "echo by setup > installed"));
const broken = await createThread("$ echo started", repository("broken", "echo setup broke >&2; exit 3"));
const [installed] = await runEnded(ready.id, "$ cat installed");
if (installed?.error || installed?.output.trim() !== "by setup") await fail(`after setup: ${JSON.stringify(installed)}`);
log("7. a repository's .agents/setup ran in its clone before the first message", installed.output.trim());
const [started] = await runEnded(broken.id, "$ echo started");
if (started?.output.trim() !== "started") await fail(`after a failed setup: ${JSON.stringify(started)}`);
log("8. a Thread whose setup exits 3 still starts and answers");
for (const t of [ready, broken]) if ((await call("DELETE", `/threads/${t.id}`)).status !== 204) await fail("delete");

await pass();
