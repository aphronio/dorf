import { call, createThread, fail, follow, killAgent, log, runEnded, send, startAgent, until } from "./kit.ts";

type Entry = { id: number; kind: string; model?: { content?: string | { text?: string }[] }[] };
const text = (e: Entry) => [e.model?.[0]?.content ?? []].flat().map((c) => (typeof c === "string" ? c : (c.text ?? ""))).join("");

await startAgent();
const first = "$ true";
const { id } = await createThread(first);
await runEnded(id, first);
// Five rounds of twelve commands: about 130 entries, more than the view's window of fifty and its most of a hundred.
for (let round = 1; round <= 5; round++) {
	const message = Array.from({ length: 12 }, (_, i) => `$ echo ${round}-${i}`).join("\n");
	await send(id, message);
	await runEnded(id, message);
}

const stream = follow(`/threads/${id}/events`);
const opened: { entries: Entry[]; earlier?: boolean; id?: number } = await until("the first event", async () => stream.events[0]);
stream.close();
if (opened.entries.length !== 50 || opened.earlier !== true || opened.id !== opened.entries.at(-1)?.id)
	await fail(`first event: ${opened.entries.length} entries, earlier ${opened.earlier}, id ${opened.id}`);
let entries = opened.entries;
for (let earlier = true; earlier; ) {
	const page = await call("GET", `/threads/${id}/entries?before=${entries[0]!.id}`);
	if (page.status !== 200) await fail(`load earlier: ${page.status}`);
	entries = [...page.json.entries, ...entries];
	earlier = page.json.earlier;
}
const ordered = entries.every((e, i) => i === 0 || e.id > entries[i - 1]!.id);
const user = entries.filter((e) => e.kind === "pi.user").map(text);
if (!ordered || user[0] !== first || user.length !== 6 || text(entries.at(-1)!) !== "done") await fail(`history: ordered ${ordered}, user messages ${JSON.stringify(user)}`);
log("1. a long Thread streams its newest 50 entries; load earlier pages the rest back to its first message", `${entries.length} entries`);

// A reconnect sends the newest entry the page has and gets exactly the ones after it, however many it missed.
for (const missed of [10, entries.length - 3]) {
	const has = entries.at(-1 - missed)!.id;
	const resumed = follow(`/threads/${id}/events`, has);
	const event: { entries: Entry[]; earlier?: boolean } = await until(`the resume after ${missed} missed`, async () => resumed.events[0]);
	resumed.close();
	const ids = (list: Entry[]) => list.map((e) => e.id).join();
	if (ids(event.entries) !== ids(entries.slice(-missed)) || "earlier" in event) await fail(`resume after ${missed} missed: ${event.entries.length} entries, earlier ${event.earlier}`);
}
log("2. a reconnect resumes right after the newest entry the page has, a drop longer than the window included");

if ((await call("DELETE", `/threads/${id}`)).status !== 204) await fail("delete");
killAgent();
console.log("PASS");
