import { call, createThread, fail, follow, killAgent, log, pass, startAgent, until } from "./kit.ts";

type Listed = { id: number; createdAt: string; starting?: true; failure?: string; working?: true; attention?: "done" | "failed"; askedAt?: string; startedAt?: string; endedAt?: string };

await startAgent();
let list = follow("/threads/events");
const state = (id: number) => list.events.at(-1)?.threads?.find((t: Listed) => t.id === id) as Listed | undefined;
const reaches = (what: string, id: number, ok: (t: Listed) => boolean) => until(what, async () => state(id) && ok(state(id)!));

const { id } = await createThread("$ sleep 2 && echo slow");
await reaches("working", id, (t) => Boolean(t.working));
log("1. the list stream shows the Thread working while its run is live");

await reaches("done", id, (t) => !t.working && t.attention === "done");
const first = state(id)!;
if (!first.startedAt || !first.endedAt || first.endedAt < first.startedAt) await fail(`run times: ${JSON.stringify(first)}`);
log("2. once the run ends it asks for a look, with when the run started and ended", JSON.stringify(state(id)));

if ((await call("POST", `/threads/${id}/seen`)).status !== 202) await fail("seen refused");
await reaches("seen", id, (t) => !t.working && !t.attention);
log("3. opening the Thread clears it");

await call("POST", `/threads/${id}/messages`, { text: "$ sleep 30" });
await reaches("working again", id, (t) => Boolean(t.working));
if (!state(id)!.askedAt || state(id)!.askedAt! < first.endedAt!) await fail(`a message did not set askedAt: ${JSON.stringify(state(id))}`);
log("   a message records when the owner last asked");
await call("POST", `/threads/${id}/abort`);
await reaches("stopped", id, (t) => !t.working);
await new Promise((r) => setTimeout(r, 500));
if (state(id)?.attention) await fail(`a stopped run asks for a look: ${JSON.stringify(state(id))}`);
log("4. a run the owner stopped asks for nothing");

await call("POST", `/threads/${id}/messages`, { text: "$ sleep 3 && echo after" });
await reaches("working before the kill", id, (t) => Boolean(t.working));
list.close();
killAgent("SIGKILL");
await startAgent();
list = follow("/threads/events");
await reaches("done after restart", id, (t) => !t.working && t.attention === "done");
log("5. a run cut off by a restart still ends as done in the list");

const broken = await createThread("$ echo before\n! the request was malformed");
await reaches("failed", broken.id, (t) => !t.working && t.attention === "failed");
if (!state(broken.id)!.endedAt) await fail(`a failed run has no end time: ${JSON.stringify(state(broken.id))}`);
log("6. a run the model fails asks for a look as failed", JSON.stringify(state(broken.id)));

// The stand-in's Sandboxes inherit this environment, so no clone over https gets out and the start fails offline.
process.env.GIT_ALLOW_PROTOCOL = "file";
const unclonable = await createThread("$ echo never", "example/unclonable");
await reaches("start failed", unclonable.id, (t) => !t.starting && t.attention === "failed");
if (!state(unclonable.id)!.failure?.includes("git clone example/unclonable") || state(unclonable.id)!.startedAt) await fail(`failed start: ${JSON.stringify(state(unclonable.id))}`);
list.close();
log("7. a Thread whose repository fails to clone asks for a look as failed, without a run", state(unclonable.id)!.failure!.split("\n")[0]);

await pass();
