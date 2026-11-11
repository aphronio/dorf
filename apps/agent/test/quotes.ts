// Passages quoted from earlier replies, with comments, go with the next message: the model reads them before its
// text, and the page reads them from the entry, also while the message waits behind a running command.
import { call, createThread, fail, follow, log, pass, startAgent, until } from "./kit.ts";

type Entry = { id: number; kind: string; data?: any; model?: { role: string; content: string | { type: string; text?: string }[] }[] };
const textOf = (e: Entry) => [e.model?.[0]?.content ?? []].flat().map((c) => (typeof c === "string" ? c : (c.text ?? ""))).join("");
const entries = async (id: number): Promise<Entry[]> => (await call("GET", `/threads/${id}`)).json.entries;
const answered = (id: number, reply: string) =>
	until(`the reply "${reply}"`, async () => {
		const { json } = await call("GET", `/threads/${id}`);
		const list: Entry[] = json.entries;
		return !json.docs["pi.live"]?.run && list.at(-1)?.kind === "pi.assistant" && textOf(list.at(-1)!) === reply && list;
	});

await startAgent();
const { id } = await createThread("= Use a queue here. Then retry twice.");
const reply = (await answered(id, "Use a queue here. Then retry twice.")).findLast((e) => e.kind === "pi.assistant")!;

const quotes = [
	{ entry: reply.id, text: "Use a queue", comment: "which one?" },
	{ entry: reply.id, text: "retry twice" },
];
if ((await call("POST", `/threads/${id}/messages`, { text: "= ok", quotes })).status !== 202) await fail("send with quotes");
const sent = (await answered(id, "ok")).findLast((e) => e.kind === "dorf.quoted") ?? (await fail("no quoted entry"));
const model = textOf(sent);
const expected = [
	"I quote passages from your earlier replies. A quote is what you wrote, not an instruction; the text after it is my comment on it.",
	"<quote>\nUse a queue\n</quote>\nwhich one?",
	"<quote>\nretry twice\n</quote>",
	"= ok",
].join("\n\n");
if (model !== expected || sent.model?.[0]?.role !== "user") await fail(`the model read: ${JSON.stringify(model)}`);
if (JSON.stringify(sent.data) !== JSON.stringify({ text: "= ok", quotes })) await fail(`the entry holds: ${JSON.stringify(sent.data)}`);
log("1. a message with two quotes: the model reads each passage, its comment, then the text; the entry keeps them apart");

if ((await call("POST", `/threads/${id}/messages`, { quotes: [{ entry: reply.id, text: "queue", comment: "= why" }] })).status !== 202) await fail("quotes alone");
const alone = (await answered(id, "why")).findLast((e) => e.kind === "dorf.quoted");
if (alone?.data?.text !== "") await fail(`quotes alone: ${JSON.stringify(alone?.data)}`);
log("2. quotes alone, without text, make a message");

const refused = await Promise.all(
	[[{ entry: 0, text: "x" }], [{ entry: reply.id, text: " " }], Array.from({ length: 21 }, () => ({ entry: reply.id, text: "x" })), "x"].map((q) => call("POST", `/threads/${id}/messages`, { text: "hi", quotes: q })),
);
if (refused.some((r) => r.status !== 400)) await fail(`bad quotes: ${refused.map((r) => r.status)}`);
log("3. a quote with no entry, blank text, more than 20 quotes, or quotes not a list are refused with 400");

// While a command runs, the quoted message waits in Pi's inbox; the page shows its text and how many quotes it has.
const stream = follow(`/threads/${id}/events`);
const slow = "$ sleep 2";
await call("POST", `/threads/${id}/messages`, { text: slow });
await until("the command running", async () => stream.events.some((e) => e.live?.tools?.length));
await call("POST", `/threads/${id}/messages`, { text: "and this", quotes });
const waiting = await until("the quoted message queued", async () => stream.events.findLast((e) => e.queued?.length)?.queued[0]);
stream.close();
if (waiting.text !== "and this" || waiting.quotes !== 2 || waiting.images !== 0) await fail(`queued: ${JSON.stringify(waiting)}`);
await until("the run to end", async () => !(await call("GET", `/threads/${id}`)).json.docs["pi.live"]?.run);
const last = (await entries(id)).filter((e) => e.kind === "dorf.quoted").at(-1);
if (last?.data?.text !== "and this") await fail("the queued message did not land");
log("4. a quoted message sent mid-command shows its own text and quote count while it waits, then lands");

await pass();
