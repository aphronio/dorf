// What only Substrate makes good on for repository snapshots: a tag of a paused Sandbox, a fork of it that holds its
// files and processes, a reaper that spares a tag until its fork saved its own snapshot, and a refusal of a tag taken
// under an older template. test/repo-snapshots.ts proves the agent's use of them against the stand-ins.
import { execFileSync } from "node:child_process";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { reap } from "../src/repo-snapshots.ts";
import { SnapshotRefused } from "../src/sandbox.ts";
import { fail, log, needSubstrate, pass, sandboxes, until } from "./kit.ts";

await needSubstrate();

const ate = (...args: string[]) => execFileSync("kubectl", ["ate", ...args, "-a", "dorf"], { encoding: "utf8", stdio: ["pipe", "pipe", "ignore"] });
const json = (...args: string[]) => JSON.parse(ate("get", ...args, "-o", "json"));
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

// The gate creates from the newest template, so a copy of the current one makes every older tag stale.
const older = `${prefix}b`;
await sandboxes.snapshot(older, source);
const current = (json("actor-templates").actorTemplates as any[]).filter((t) => t.metadata.name.startsWith("sandbox-")).sort((a, b) => Number(b.metadata.createTime.seconds) - Number(a.metadata.createTime.seconds))[0];
const { status: _status, metadata, ...spec } = current;
const copy = `${metadata.name}-p${Date.now().toString(36)}`;
execFileSync("kubectl", ["ate", "create", "actor-template", "-f", "-"], { input: JSON.stringify({ ...spec, metadata: { atespace: metadata.atespace, name: copy } }), stdio: ["pipe", "ignore", "inherit"] });
await until("the copied template's golden snapshot", async () => !!json("actor-template", copy).status?.goldenSnapshotStatus?.goldenTag?.name, 10 * 60_000);
const stale = sandboxes.newName();
made.push(stale);
const refused = await sandboxes.create(stale, older).then(() => undefined, (e) => e);
if (!(refused instanceof SnapshotRefused)) await fail(`a tag from template ${metadata.name} under ${copy}: ${refused?.message ?? "created"}`);
log("5. under a newer template the gate refuses the older tag with 412", refused.message.slice(0, 120));

for (const name of made) await sandboxes.delete(name);
for (const s of await ours()) await sandboxes.deleteSnapshot(s.name);
// A Sandbox that is not a proof's may have started on the copy meanwhile; deploy/up.sh deletes it once unused.
const used = (json("actors").actors ?? []).some((a: any) => a.actorTemplate?.name === copy);
if (!used) ate("delete", "actor-template", copy);
await pass();
