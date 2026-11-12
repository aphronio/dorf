import { call, createThread, fail, follow, log, pass, runEnded, send, startAgent, until } from "./kit.ts";

type Entry = { id: number; kind: string; model?: { content?: string | { text?: string }[] }[] };
const text = (e: Entry) => {
	const content = e.model?.[0]?.content ?? [];
	return Array.isArray(content) ? content.map((c) => c.text ?? "").join("") : content;
};

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
const pages = [opened.entries];
for (let earlier = true; earlier; ) {
	const page = await call("GET", `/threads/${id}/entries?before=${pages.at(-1)![0]!.id}`);
	if (page.status !== 200) await fail(`load earlier: ${page.status}`);
	pages.push(page.json.entries);
	earlier = page.json.earlier;
}
const entries = pages.reverse().flat();
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

// The list stream sends the list on connect, then each change: a Thread created elsewhere, its work starting and ending.
type Listed = { id: number; title: string; working?: true };
const list = follow("/threads/events");
const now: { threads: Listed[] } = await until("the list on connect", async () => list.events[0]);
if (now.threads.map((t) => t.id).join() !== String(id) || now.threads[0]!.working) await fail(`list on connect: ${JSON.stringify(now.threads)}`);
const started = await createThread("$ sleep 1");
// The new Thread's state in each event after the first, repeats folded: "idle → working → idle → gone".
const trail = () =>
	list.events
		.slice(1)
		.map((e) => e.threads.find((t: Listed) => t.id === started.id))
		.map((t) => (t === undefined ? "gone" : t.working ? "working" : "idle"))
		.filter((state, i, all) => i === 0 || state !== all[i - 1])
		.join(" → ");
await until("the new Thread listed, working, then idle", async () => /^(idle → )?working → idle$/.test(trail()));
if (!list.events.slice(1).some((e) => e.threads.some((t: Listed) => t.id === started.id && t.title === "$ sleep 1"))) await fail("the new Thread's title");
if ((await call("DELETE", `/threads/${started.id}`)).status !== 204) await fail("delete");
await until("the deleted Thread to leave the list", async () => trail().endsWith("idle → gone"));
list.close();
log("3. the list stream sends the list on connect, a Thread created through POST /threads, its work starting and ending, and its deletion", trail());

if ((await call("DELETE", `/threads/${id}`)).status !== 204) await fail("delete");
await pass();
