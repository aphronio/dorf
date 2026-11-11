import { call, createThread, fail, follow, killAgent, log, pass, startAgent, until } from "./kit.ts";

type Listed = { id: number; working?: true; attention?: "done" | "failed" };

await startAgent();
let list = follow("/threads/events");
const state = (id: number) => list.events.at(-1)?.threads?.find((t: Listed) => t.id === id) as Listed | undefined;
const reaches = (what: string, id: number, ok: (t: Listed) => boolean) => until(what, async () => state(id) && ok(state(id)!));

const { id } = await createThread("$ sleep 2 && echo slow");
await reaches("working", id, (t) => Boolean(t.working));
log("1. the list stream shows the Thread working while its run is live");

await reaches("done", id, (t) => !t.working && t.attention === "done");
log("2. once the run ends it asks for a look", JSON.stringify(state(id)));

if ((await call("POST", `/threads/${id}/seen`)).status !== 202) await fail("seen refused");
await reaches("seen", id, (t) => !t.working && !t.attention);
log("3. opening the Thread clears it");

await call("POST", `/threads/${id}/messages`, { text: "$ sleep 30" });
await reaches("working again", id, (t) => Boolean(t.working));
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
list.close();
log("5. a run cut off by a restart still ends as done in the list");

await pass();
