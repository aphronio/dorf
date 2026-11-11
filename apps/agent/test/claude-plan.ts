// Needs Claude Code signed in to a Claude plan on this machine; set DORF_CLAUDE_BIN to use another claude binary.
import { execFileSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { getSessionMessages } from "@anthropic-ai/claude-agent-sdk";
import { agentPid, call, createThread, DATA, fail, IDLE_MS, killAgent, log, pass, sandboxes, send, startAgent, until } from "./kit.ts";

const MODEL = "claude-plan/opus";
const RECORDS = `${DATA}/claude/sessions`;
type Usage = { input: number; output: number; cacheRead: number; cacheWrite: number };
type Msg = { role: string; content?: string | { type: string; text?: string }[]; usage?: Usage; stopReason?: string; errorMessage?: string };
type Entry = { kind: string; model?: Msg[] };

const text = (m?: Msg) => (typeof m?.content === "string" ? m.content : (m?.content ?? []).map((c) => c.text ?? "").join(""));
const steps = (usage: Usage[]) => usage.map((u) => `read ${u.cacheRead}/write ${u.cacheWrite}`).join(", ");
const claudeSession = () => JSON.parse(readFileSync(`${RECORDS}/${readdirSync(RECORDS)[0]}`, "utf8")).claude as string;
const claudePids = () =>
	execFileSync("ps", ["-e", "-o", "pid=,ppid=,comm="])
		.toString()
		.split("\n")
		.map((line) => line.trim().split(/\s+/))
		.filter(([, ppid, comm]) => ppid === String(agentPid()) && comm === "claude")
		.map(([pid]) => pid);
const claudeChildren = () => claudePids().length;

async function entries(id: number): Promise<{ entries: Entry[]; running: boolean }> {
	const { json } = await call("GET", `/threads/${id}`);
	return { entries: json.entries, running: Boolean(json.docs["pi.live"]?.run) };
}

async function answer(id: number, message: string) {
	return until(
		`the answer to ${JSON.stringify(message.slice(0, 50))}`,
		async () => {
			const view = await entries(id);
			const asked = view.entries.findLastIndex((e) => e.kind === "pi.user" && text(e.model?.[0]) === message);
			if (asked < 0 || view.running) return undefined;
			const after = view.entries.slice(asked + 1);
			const replies = after.filter((e) => e.kind === "pi.assistant").map((e) => e.model![0]);
			const failed = replies.find((r) => r.stopReason === "error" || r.stopReason === "aborted");
			if (failed) return fail(`model: ${failed.errorMessage}`);
			if (replies.at(-1)?.stopReason !== "stop") return undefined;
			return { reply: text(replies.at(-1)), tools: after.filter((e) => e.kind === "pi.tool-result").map((e) => text(e.model![0])), usage: replies.map((r) => r.usage!) };
		},
		180_000,
	);
}

await startAgent(MODEL);

const first = "Use the bash tool to run `uname -sr`, then reply with only the kernel release.";
const { id, sandbox } = await createThread(first);
const a = await answer(id, first);
const release = a.tools[0]?.match(/^Linux (\S+)/)?.[1];
if (!release) await fail(`first turn: ${JSON.stringify(a)}`);
log("1. answered on the Claude plan, bash ran in the Sandbox", `reply ${JSON.stringify(a.reply)}; ${steps(a.usage)}`);

const second = "In one reply, make two separate bash tool calls: `echo warm-$((6*7))` and `echo second-call`. Then reply with only both outputs.";
await send(id, second);
const b = await answer(id, second);
if (!b.tools.some((t) => t.includes("warm-42")) || !b.tools.some((t) => t.includes("second-call"))) await fail(`second turn: ${JSON.stringify(b)}`);
if (!b.usage.every((u) => u.cacheRead > 0)) await fail(`second turn did not read the cache: ${steps(b.usage)}`);
const transcript = JSON.stringify(await getSessionMessages(claudeSession(), { dir: `${DATA}/claude` }));
if (transcript.includes("Cancelled.")) await fail("a tool call was cancelled and replayed instead of answered in place");
log("2. warm turn read the cache; parallel tool calls answered in place", `${b.tools.length} calls; ${steps(b.usage)}`);

const { json: offered } = (await call("GET", "/models")) as { json: { ref: string; name: string; thinking: string[] }[] };
const real = ((await entries(id)).entries.findLast((e) => e.kind === "pi.assistant")?.model?.[0] as { responseModel?: string })?.responseModel;
if (offered[0]?.ref !== MODEL || !offered[0].thinking.includes("high") || !real?.startsWith("claude-"))
	await fail(`models ${JSON.stringify(offered)}, reply by ${real}`);
log("2b. the model list comes from Claude Code; replies name the model that wrote them", `${offered.map((m) => `${m.name} [${m.thinking.join(" ")}]`).join(", ")}; ${real}`);

const session = claudeSession();
const [claudePid] = claudePids();
if ((await call("POST", `/threads/${id}/thinking`, { level: "low" })).status !== 202) await fail("thinking change");
const lighter = "Use the bash tool to run `echo light`, then reply with only its output.";
await send(id, lighter);
if (!(await answer(id, lighter)).tools.some((t) => t.includes("light"))) await fail("turn after the thinking change");
const base = "Use the bash tool to run `sleep 8 && echo base-done`, then reply with only its output.";
await send(id, base);
await until("the base call to start", async () => {
	const view = await entries(id);
	const asked = view.entries.findLastIndex((e) => e.kind === "pi.user" && text(e.model?.[0]) === base);
	return asked >= 0 && view.entries.slice(asked).some((e) => e.kind === "pi.assistant" && JSON.stringify(e.model).includes('"toolCall"'));
});
const steer = "Also end your reply with the word pineapple.";
await send(id, steer);
const steered = await answer(id, steer);
const kept = JSON.stringify(await getSessionMessages(claudeSession(), { dir: `${DATA}/claude` }));
const level = ((await entries(id)).entries.findLast((e) => e.kind === "pi.assistant")?.model?.[0] as { providerThinkingLevel?: string })?.providerThinkingLevel;
// Read as the user's, the steer is obeyed; read as tool output, it is only mentioned.
if (!/pineapple\W*$/i.test(steered.reply.trim()) || kept.includes("Cancelled.") || claudeSession() !== session || claudePids()[0] !== claudePid || level !== "low")
	await fail(`steer: reply ${JSON.stringify(steered.reply)}, cancelled ${kept.includes("Cancelled.")}, same session ${claudeSession() === session}, same process ${claudePids()[0] === claudePid}, effort ${level}`);
log("2c. thinking changed and a steer landed mid-turn, both in the same running Claude Code process", `reply ${JSON.stringify(steered.reply)}`);

const slow = "Use the bash tool to run `echo run >> runs && sleep 8 && echo slow-done`, then reply with only its last line.";
await send(id, slow);
await until("the slow call to start", async () => {
	const view = await entries(id);
	const asked = view.entries.findLastIndex((e) => e.kind === "pi.user" && text(e.model?.[0]) === slow);
	return asked >= 0 && view.entries.slice(asked).some((e) => e.kind === "pi.assistant" && JSON.stringify(e.model).includes('"toolCall"'));
});
await new Promise((r) => setTimeout(r, 2_000));
killAgent("SIGKILL");
log("3. agent killed mid-call");
await startAgent(MODEL);
const c = await answer(id, slow);
if (!c.tools.some((t) => t.includes("slow-done"))) await fail(`after restart: ${JSON.stringify(c)}`);
const count = "Use the bash tool to run `wc -l < runs`, then reply with only the number.";
await send(id, count);
const ran = await answer(id, count);
if (ran.tools[0]?.trim() !== "1") await fail(`the killed call ran ${ran.tools[0]?.trim()} times`);
log("   the call finished after restart and ran once", steps(c.usage));

// Pi keeps the newest 20,000 tokens verbatim, so compaction needs a longer Thread than these short turns.
const filler = `Reply with only OK. Ignore this filler: ${"lorem ipsum dolor sit amet ".repeat(4_000)}`;
await send(id, filler);
await answer(id, filler);
const before = claudeSession();
const compacted = await call("POST", `/threads/${id}/compact`);
if (compacted.status !== 202) await fail(`compact: ${compacted.status}`);
await until("the compaction summary", async () => (await entries(id)).entries.some((e) => e.kind === "pi.compaction"), 180_000);
const recall = "Without using any tool, reply with only the kernel release you found at the start.";
await send(id, recall);
const d = await answer(id, recall);
if (!d.reply.includes(release!)) await fail(`after compaction the reply was ${JSON.stringify(d.reply)}, not ${release}`);
if (claudeSession() === before) await fail("the turn after compaction did not start a new Claude Code session");
log("4. manual compaction: the next turn ran in a new Claude Code session and remembered", `reply ${JSON.stringify(d.reply)}; ${steps(d.usage)}`);

const delegate =
	'Use the subagent tool to spawn a subagent named helper with the message "Use the bash tool to run `echo sub-ok`, then reply with only its output." Then reply with only the word spawned.';
await send(id, delegate);
await answer(id, delegate);
const report = await until("the subagent's report", async () =>
	(await entries(id)).entries.find((e) => e.kind === "dorf.subagent-report" && text(e.model?.[0]).startsWith("[subagent helper answered")),
	180_000,
);
if (!text(report.model?.[0]).includes("sub-ok")) await fail(`subagent report: ${text(report.model?.[0])}`);
await until("the Thread to finish with the report", async () => !(await entries(id)).running, 180_000);
if (claudeChildren() !== 2) await fail(`${claudeChildren()} Claude Code processes, not one each for the Thread and its subagent`);
log("4b. a subagent ran as its own Claude Code session in the Thread's Sandbox and reported back", text(report.model?.[0]));

await until("idle pause", async () => (await sandboxes.state(sandbox)) === "paused", IDLE_MS + 60_000);
if (claudeChildren()) await fail("Claude Code still runs for the Thread or its subagent after the Thread went idle");
const resumed = "Use the bash tool to run `echo resumed`, then reply with only its output.";
await send(id, resumed);
const e = await answer(id, resumed);
if (!e.tools.some((t) => t.includes("resumed"))) await fail(`after idle: ${JSON.stringify(e)}`);
log("5. idle stopped Claude Code with the Sandbox; the next message resumed its session", steps(e.usage));

if ((await call("DELETE", `/threads/${id}`)).status !== 204) await fail("delete");
if (existsSync(RECORDS) && readdirSync(RECORDS).length) await fail("a Claude Code record of the Thread or its subagent survived its deletion");
log("6. thread deleted with its Claude Code record");

await pass();
