// What only Substrate makes good on for repository snapshots: a tag of a paused Sandbox, a fork of it that holds its
// files and processes, and a reaper that spares a tag until its fork saved its own snapshot. test/repo-snapshots.ts
// proves the agent's use of them against the stand-ins.
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { reap } from "../src/repo-snapshots.ts";
import { fail, log, needSubstrate, pass, sandboxes, until } from "./kit.ts";

await needSubstrate();

const prefix = sandboxes.named("snap-");
const ours = async () => (await sandboxes.snapshots()).filter((s) => s.name.startsWith(prefix));
const made: string[] = [];

async function run(sandbox: string, script: string): Promise<string> {
	let output = "";
	const ran = await sandboxes.env(sandbox, "/workspace").exec(["sh", "-c", script], { onOutput: (text) => void (output += text) }, context);
	if (!ran.ok) await fail(`${sandbox}: ${ran.error.message}`);
	return output.trim();
}

async function sandbox(from?: string): Promise<string> {
	const name = sandboxes.newName();
	made.push(name);
	await sandboxes.create(name, from);
	if (!(await sandboxes.wake(name, false))) await fail(`no room for ${name}`);
	return name;
}

// Tags a killed run left; proof.sh deletes its Sandboxes.
for (const s of await ours()) await sandboxes.deleteSnapshot(s.name);

const source = await sandbox();
await run(source, "echo kept > note && (sleep 100000 & echo $! > pid) && echo started");
await sandboxes.pause(source, "save");
const tag = `${prefix}a`;
await sandboxes.snapshot(tag, source);
const listed = (await ours()).find((s) => s.name === tag);
if (!listed?.ready || listed.borrowed) await fail(`tag after CreateTag: ${JSON.stringify(listed)}`);
log("1. a paused Sandbox's tag is ready when CreateTag returns, and nothing borrows it", tag);

const fork = await sandbox(tag);
const seen = await run(fork, "cat note && kill -0 $(cat pid) && echo alive");
if (seen !== "kept\nalive") await fail(`fork: ${JSON.stringify(seen)}`);
log("2. a Sandbox created from the tag holds its file and its process", fork);

if (!(await ours()).find((s) => s.name === tag)?.borrowed) await fail("the running fork does not borrow its tag");
if ((await reap(sandboxes, prefix, undefined)).length) await fail("the reaper deleted a borrowed tag");
log("3. while the fork has saved nothing of its own, it borrows the tag and the reaper spares it");

await sandboxes.pause(fork, "save");
await until("the tag to be free", async () => !(await ours()).find((s) => s.name === tag)?.borrowed);
if ((await reap(sandboxes, prefix, undefined)).join() !== tag) await fail("the reaper kept a tag nothing borrows");
await sandboxes.wake(fork, false);
if ((await run(fork, "cat note")) !== "kept") await fail("the fork lost its file with the tag");
log("4. after the fork's first sync pause the tag is free; the reaper deleted it and the fork resumed intact");

for (const name of made) await sandboxes.delete(name);
for (const s of await ours()) await sandboxes.deleteSnapshot(s.name);
await pass();
