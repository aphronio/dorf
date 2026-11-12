// Drives one Thread's subagents for the proofs that need them: models.ts and subagent-restarts.ts.
import { call, fail, follow, killAgent, send, startAgent, until } from "./kit.ts";

export type Entry = { kind: string; data?: { name: string; failed?: true }; model?: { role: string; model?: string; isError?: boolean; content?: string | { type: string; text?: string }[] }[] };

export const text = (e: Entry) => (e.model ?? []).flatMap((m) => (typeof m.content === "string" ? [m.content] : (m.content ?? []).map((c) => c.text ?? ""))).join("");
export const subagent = (args: object) => `@ ${JSON.stringify(args)}`;
export const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
export const TOGETHER = (name: string) => `${name} answered this message together with another one, in the answer another call returned.`;
export const STOPPED_WAITING = (name: string) => `Stopped waiting because the owner sent a message. ${name} is still working; its answer will arrive as a message.`;

export async function subagentThread(id: number) {
	const entries = async () => (await call("GET", `/threads/${id}`)).json.entries as Entry[];
	const reports = async (name: string) => (await entries()).filter((e) => e.kind === "dorf.subagent-report" && e.data?.name === name && !e.data.failed).map(text);
	const results = async () => (await entries()).filter((e) => e.kind === "pi.tool-result").map((e) => ({ output: text(e), error: Boolean(e.model?.[0]?.isError) }));

	// A Thread's turn: from its message to the next one, once the Thread answered "done".
	async function answered(message: string, timeoutMs?: number): Promise<{ output: string; error: boolean }[]> {
		return until(
			`the answer to ${JSON.stringify(message.slice(0, 50))}`,
			async () => {
				const all = await entries();
				const asked = all.findLastIndex((e) => e.kind === "pi.user" && text(e) === message);
				if (asked < 0) return undefined;
				const next = all.findIndex((e, i) => i > asked && e.kind === "pi.user");
				const turn = all.slice(asked + 1, next < 0 ? undefined : next);
				if (!turn.some((e) => e.kind === "pi.assistant" && text(e) === "done")) return undefined;
				return turn.filter((e) => e.kind === "pi.tool-result").map((e) => ({ output: text(e), error: Boolean(e.model?.[0]?.isError) }));
			},
			timeoutMs,
		);
	}
	async function ask(message: string) {
		await send(id, message);
		return answered(message);
	}

	let stream = follow(`/threads/${id}/events`);
	await until("the stream", async () => stream.events.length > 0);
	const working = (name: string) => stream.events.findLast((e) => e.subagents)?.subagents?.find((s: { name: string }) => s.name === name)?.working === true;

	// Kills the agent and starts it again.
	async function restart() {
		stream.close();
		killAgent("SIGKILL");
		await startAgent();
		stream = follow(`/threads/${id}/events`);
		await until("the stream", async () => stream.events.length > 0);
	}
	// Sends a message to a subagent, then the owner's message that ends the Thread's wait for it.
	async function handOff(name: string, message: string) {
		await send(id, subagent({ name, message }));
		await until(`${name} to work`, async () => working(name), 30_000);
		await ask("$ true");
		if (!(await results()).some((r) => r.output === STOPPED_WAITING(name))) await fail(`${name} was not handed off: ${JSON.stringify(await results())}`);
	}

	return { entries, reports, results, answered, ask, working, restart, handOff, events: () => stream.events };
}
