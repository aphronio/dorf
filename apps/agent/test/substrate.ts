// What only Substrate makes good on; the other proofs run the same agent against local stand-ins.
import { call, createThread, fail, IDLE_MS, log, needSubstrate, pass, runEnded, sandboxes, send, startAgent, until } from "./kit.ts";

await needSubstrate();
const zombies = "grep -l 'State:.*Z' /proc/[0-9]*/status 2>/dev/null | wc -l";
// The Kubernetes API answers anyone who reaches it by its cluster name.
const cluster = `$ node -e "require('https').get({host:'kubernetes.default.svc.cluster.local',path:'/version',rejectUnauthorized:false,timeout:5000},r=>console.log('cluster answered',r.statusCode)).on('timeout',function(){this.destroy(new Error('timeout'))}).on('error',e=>console.log('cluster unreachable',e.code??e.message))"`;

await startAgent();

const prompt = `$ echo hello > note.txt && (sleep 100000 & echo $! > pid) && echo started\n$ git ls-remote https://github.com/git/git HEAD\n${cluster}`;
const { id, sandbox } = await createThread(prompt);
const first = await runEnded(id, prompt);
if (!first[0]?.output.includes("started")) await fail(`first tool call: ${JSON.stringify(first)}`);
if (!/\tHEAD/.test(first[1]?.output ?? "")) await fail(`internet: ${JSON.stringify(first[1])}`);
if (!first[2]?.output.includes("cluster unreachable")) await fail(`cluster reachable: ${JSON.stringify(first[2])}`);
log("1. file written, process started, internet reached, cluster not", `sandbox ${sandbox}`);

const look = "$ ls note.txt 2>&1 || echo none";
const other = await createThread(look);
const [seen] = await runEnded(other.id, look);
if (!seen?.output.includes("none")) await fail(`another Sandbox sees the first one's file: ${JSON.stringify(seen)}`);
log("2. another Thread's Sandbox does not see that file", `sandbox ${other.sandbox}`);

await until("pause", async () => (await sandboxes.state(sandbox)) === "paused", IDLE_MS + 60_000);
const check = `$ cat note.txt && kill -0 $(cat pid) && echo alive && echo zombies $(${zombies})`;
await send(id, check);
const [after] = await runEnded(id, check);
if (!/hello[\s\S]*alive[\s\S]*zombies 0/.test(after?.output ?? "")) await fail(`after resume: ${JSON.stringify(after)}`);
log("3. paused and resumed with file and process intact, no zombies");

for (const t of [id, other.id]) if ((await call("DELETE", `/threads/${t}`)).status !== 204) await fail("delete");
await pass();
