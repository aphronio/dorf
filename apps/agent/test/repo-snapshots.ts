import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { repoSnapshots } from "../src/repo-snapshots.ts";
import { call, createThread, DATA, fail, killAgent, log, pass, repository, runEnded, sandboxes, startAgent, until } from "./kit.ts";

// Setup counts its runs, and installs `deps` only when they changed, as a package manager's cache does, counting
// those too. `deps` names a version and how many seconds its install takes. The proof's slow catch-up of 4 s stands
// in for the deployed 10 s: an install of 5 s is slower, and a catch-up with no install took up to 2.3 s while the
// other proofs ran beside this one.
const SETUP = `#!/bin/sh
echo run >> .runs
want=$(cat deps)
[ "$(cat .installed 2>/dev/null)" = "$want" ] && exit 0
sleep \${want#* }
echo "$want" > .installed
echo install >> .installs
`;
const SLOW_CATCH_UP_MS = 4_000;
const { names } = repoSnapshots(sandboxes, 0);
const ours = async (repo: string) => (await sandboxes.snapshots()).filter((s) => s.name.startsWith(names(repo).prefix)).map((s) => s.name);
const building = async (repo: string) => (await sandboxes.state(names(repo).builder).catch(() => undefined)) !== undefined;

// A Thread on the repository answers one tool call and is deleted, so its Sandbox leaves room for a builder.
async function ask(repo: string, prompt: string): Promise<string> {
	const { id } = await createThread(prompt, repo);
	const [result] = await runEnded(id, prompt);
	if (!result || result.error) await fail(`${prompt}: ${JSON.stringify(result)}`);
	if ((await call("DELETE", `/threads/${id}`)).status !== 204) await fail("delete");
	return result!.output.trim().split(/\s+/).join(" ");
}

// Long enough that no Sandbox pauses on its own during the proof.
await startAgent("faux", 60 * 60_000, SLOW_CATCH_UP_MS);
const { repo, commit } = repository("orb", { deps: "v1 0\n", ".agents/setup": SETUP });
const look = "$ cut -d' ' -f1 .installed; wc -l < .runs; wc -l < .installs";

const first = await ask(repo, look);
if (first !== "v1 1 1") await fail(`first Thread: ${first}`);
const [v1] = await until("the first snapshot", async () => !(await building(repo)) && (await ours(repo)).length === 1 && (await ours(repo)));
log("1. the first Thread cloned and set up; a builder then saved a snapshot", v1);

const second = await ask(repo, look);
if (second !== "v1 2 1") await fail(`second Thread: ${second}`);
log("2. a second Thread started from the snapshot: setup ran again and installed nothing", "runs 2, installs 1");

commit({ "README.md": "readme\n" }, "readme");
const third = await ask(repo, "$ git log -1 --format=%s; cut -d' ' -f1 .installed; wc -l < .installs");
if (third !== "readme v1 1") await fail(`third Thread: ${third}`);
// A build either Thread started would have a builder or a new snapshot by now.
await new Promise((r) => setTimeout(r, 1_000));
if ((await building(repo)) || (await ours(repo)).join() !== v1) await fail("a Thread at the snapshot's commit, or one that caught up fast, started a build");
log("3. neither the Thread at the snapshot's commit nor one at a commit setup ignores started a build");

const tip = commit({ deps: "v2 5\n" }, "deps");
const atTip = async () => (await ours(repo)).find((n) => n.startsWith(`${names(repo).prefix}${tip.slice(0, 8)}-`));
const fourth = await ask(repo, "$ cut -d' ' -f1 .installed; wc -l < .installs");
if (fourth !== "v2 2") await fail(`fourth Thread: ${fourth}`);
await until("the build after a slow catch-up", () => building(repo));
if (await atTip()) await fail("the build finished before the agent was killed");
killAgent("SIGKILL");
await startAgent("faux", 60 * 60_000, SLOW_CATCH_UP_MS);
const v2 = await until("the snapshot at the new commit", async () => !(await building(repo)) && (await atTip()), 90_000);
log("4. a commit that changed deps caught up slowly and started a build; the agent, killed during it, finished it after restart", v2);

const fifth = await ask(repo, "$ cut -d' ' -f1 .installed; wc -l < .installs");
if (fifth !== "v2 2") await fail(`fifth Thread: ${fifth}`);
log("5. the next Thread started from the new snapshot", "installs 2: the first snapshot's and the build's");

// A quick install, so the plain Sandbox and the rebuild do not wait on it.
commit({ deps: "v3 0\n" }, "quick deps");
await sandboxes.deleteSnapshot(v2);
const sixth = await ask(repo, "$ cut -d' ' -f1 .installed; wc -l < .installs");
if (sixth !== "v3 1") await fail(`sixth Thread: ${sixth}`);
const v3 = await until("the snapshot rebuilt after a refusal", async () => {
	const names = await ours(repo);
	return !(await building(repo)) && names.length === 1 && names[0] !== v1 && names[0] !== v2 && names[0];
});
log("6. a snapshot the gate refused (412) gave a plain Sandbox, which started a build; it reaped the older snapshot", v3);

// Stand-ins share absolute paths, so each run of the failing setup, in a Thread's Sandbox or a builder's, counts here.
const runs = join(DATA, "broken-runs");
const broken = repository("broken", { ".agents/setup": `#!/bin/sh\necho run >> ${runs}\necho setup broke >&2; exit 3\n` }).repo;
const counted = () => (existsSync(runs) ? readFileSync(runs, "utf8").split("\n").length - 1 : 0);
for (const attempt of [1, 2]) {
	if ((await ask(broken, "$ echo started")) !== "started") await fail("a Thread whose setup fails did not start");
	await until(`build ${attempt} of a repository whose setup fails`, async () => counted() === 2 * attempt && !(await building(broken)));
	if ((await ours(broken)).length) await fail(`a failed setup saved ${await ours(broken)}`);
}
log("7. a setup that fails saves nothing, and the next Thread tries again");

await pass();
