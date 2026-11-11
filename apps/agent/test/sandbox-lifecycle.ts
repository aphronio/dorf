import { SandboxNotFound } from "../src/sandbox.ts";
import { call, createThread, fail, IDLE_MS, killAgent, log, runEnded, sandboxes, send, startAgent, until } from "./kit.ts";

const zombies = "grep -l 'State:.*Z' /proc/[0-9]*/status 2>/dev/null | wc -l";

await startAgent();

// The Kubernetes API answers anyone who reaches it by its cluster name.
const cluster = `$ node -e "require('https').get({host:'kubernetes.default.svc.cluster.local',path:'/version',rejectUnauthorized:false,timeout:5000},r=>console.log('cluster answered',r.statusCode)).on('timeout',function(){this.destroy(new Error('timeout'))}).on('error',e=>console.log('cluster unreachable',e.code??e.message))"`;
const prompt = `$ echo hello > note.txt && (sleep 100000 & echo $! > pid) && echo started\n$ git ls-remote https://github.com/git/git HEAD\n${cluster}`;
const { id, sandbox } = await createThread(prompt);
const first = await runEnded(id, prompt);
if (!first[0]?.output.includes("started")) await fail(`first tool call: ${JSON.stringify(first)}`);
if (!/\tHEAD/.test(first[1]?.output ?? "")) await fail(`internet: ${JSON.stringify(first[1])}`);
if (!first[2]?.output.includes("cluster unreachable")) await fail(`cluster reachable: ${JSON.stringify(first[2])}`);
log("1. thread created, file written, process started, internet reached, cluster not", `thread ${id}, sandbox ${sandbox}`);

const ranAt = Date.now();
await until("pause", async () => (await sandboxes.state(sandbox)) === "paused", IDLE_MS + 60_000);
log("2. sandbox paused", `${((Date.now() - ranAt) / 1000).toFixed(1)}s after the run (idle window ${IDLE_MS / 1000}s)`);

const sentAt = Date.now();
const check = `$ cat note.txt && kill -0 $(cat pid) && echo alive && echo zombies $(${zombies})`;
await send(id, check);
const second = await runEnded(id, check);
if (!/hello[\s\S]*alive[\s\S]*zombies 0/.test(second[0]?.output ?? "")) await fail(`after resume: ${JSON.stringify(second)}`);
log("3. resumed with file and process intact, no zombies", `message to answer ${Date.now() - sentAt} ms`);

const slow = "$ echo run >> runs && sleep 5 && echo slow-done";
await send(id, slow);
await new Promise((r) => setTimeout(r, 2_000));
killAgent("SIGKILL");
log("4. agent killed mid-call");
await startAgent();
const [third] = await runEnded(id, slow);
if (third?.error || !third?.output.includes("slow-done")) await fail(`killed call: ${JSON.stringify(third)}`);
const runs = "$ wc -l < runs";
await send(id, runs);
const [ran] = await runEnded(id, runs);
if (ran?.output.trim() !== "1") await fail(`the killed call ran ${ran?.output.trim()} times`);
log("   the call finished after restart", "its command ran once");

const timed = "$[2] sleep 61 && echo never";
await send(id, timed);
const [timedOut] = await runEnded(id, timed);
if (!timedOut?.error || !/timed out/.test(timedOut.output)) await fail(`timeout: ${JSON.stringify(timedOut)}`);
const left = String.raw`$ for f in /proc/[0-9]*/cmdline; do tr '\0' ' ' <$f; echo; done 2>/dev/null | grep -c '^sleep 61 $' || true`;
await send(id, left);
const [stillRunning] = await runEnded(id, left);
if (stillRunning?.output.trim() !== "0") await fail(`timed-out command still running: ${JSON.stringify(stillRunning)}`);
log("5. timeout failed the call and killed its command");

const busy = "$[60] sleep 30 && echo never";
await send(id, busy);
await new Promise((r) => setTimeout(r, 2_000));
const archivedAt = Date.now();
if ((await call("POST", `/threads/${id}/archive`)).status !== 202) await fail("archive refused");
await until("the archive's pause", async () => (await sandboxes.state(sandbox)) === "paused", IDLE_MS / 2);
const listed = async () => (await call("GET", "/threads")).json.find((t: { id: number }) => t.id === id);
if (!(await listed())?.archived) await fail("the archived thread is not marked archived");
log("6. archive stopped the work and paused the sandbox at once", `${Date.now() - archivedAt} ms, idle window ${IDLE_MS / 1000}s`);

const back = "$ cat note.txt";
await send(id, back);
const [again] = await runEnded(id, back);
if (again?.output.trim() !== "hello" || (await listed())?.archived) await fail(`after archive: ${JSON.stringify(again)}`);
log("7. a message brought the archived thread back with its file");

const deleted = await call("DELETE", `/threads/${id}`);
if (deleted.status !== 204) await fail(`delete: ${deleted.status}`);
const gone = await sandboxes.state(sandbox).then(
	() => false,
	(e) => e instanceof SandboxNotFound,
);
if (!gone) await fail(`sandbox ${sandbox} still exists after delete`);
if ((await call("GET", `/threads/${id}`)).status !== 404) await fail("deleted thread still readable");
log("8. thread deleted, sandbox gone");

killAgent();
console.log("PASS");
