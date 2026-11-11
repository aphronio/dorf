// Each `$ ` line of the newest user message becomes a bash call (`$[5] ` sets a 5 s timeout), each `> path` line a read
// of that file, and each `@ {...}` line a subagent call with those arguments; `@ [{...}, {...}]` makes several calls in one
// answer, which run at once. Then "done", the text of a `= text` line, or "done, saw 2 images" when the message carried
// images; a `! reason` line instead makes the answer a model error with that reason, which ends the run failed. A
// message of just `?` gets the system prompt's sections back, and one of just `^` the message before it.
import { fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseFactory } from "@earendil-works/pi-ai/providers/faux";

export const FAUX_MODEL = { provider: "faux", modelId: "faux" };

export function scriptedModel() {
	const faux = fauxProvider({
		provider: FAUX_MODEL.provider,
		models: [
			{ id: FAUX_MODEL.modelId, reasoning: true },
			{ id: "faux-1", name: "Faux 1", reasoning: true },
			{ id: "faux-2", name: "Faux 2", reasoning: true },
		],
	});
	const step: FauxResponseFactory = (transcript) => {
		faux.appendResponses([step]);
		const messages = transcript.messages;
		const lastUser = messages.findLastIndex((m) => m.role === "user");
		const content = messages[lastUser]?.content;
		const text = typeof content === "string" ? content : (content ?? []).map((c) => (c.type === "text" ? c.text : "")).join("");
		const sections = Object.assign({}, ...messages.map((m) => (m.role === "system" ? m.sections : undefined)));
		if (text === "?") return fauxAssistantMessage(Object.values(sections).filter(Boolean).join("\n"));
		if (text === "^") {
			const before = messages.slice(0, lastUser).findLast((m) => m.role === "user")?.content;
			return fauxAssistantMessage(`before: ${typeof before === "string" ? before : (before ?? []).map((c) => (c.type === "text" ? c.text : "")).join("")}`);
		}
		const rounds = text.split("\n").flatMap((l) => {
			const bash = l.match(/^\$(?:\[(\d+)\])? (.*)$/);
			if (bash) return [[fauxToolCall("bash", { command: bash[2], ...(bash[1] ? { timeout: Number(bash[1]) } : {}) })]];
			if (l.startsWith("> ")) return [[fauxToolCall("read", { path: l.slice(2) })]];
			if (!l.startsWith("@ ")) return [];
			const args: object | object[] = JSON.parse(l.slice(2));
			return [(Array.isArray(args) ? args : [args]).map((a) => fauxToolCall("subagent", a))];
		});
		let done = messages.slice(lastUser + 1).filter((m) => m.role === "toolResult").length;
		const next = rounds.find((round) => (done -= round.length) < 0);
		if (next) return fauxAssistantMessage(next, { stopReason: "toolUse" });
		const error = text.split("\n").findLast((l) => l.startsWith("! "))?.slice(2);
		if (error) return fauxAssistantMessage("", { stopReason: "error", errorMessage: error });
		const answer = text.split("\n").findLast((l) => l.startsWith("= "))?.slice(2);
		const images = typeof content === "string" ? 0 : (content ?? []).filter((c) => c.type === "image").length;
		return fauxAssistantMessage(answer ?? (images ? `done, saw ${images} image${images > 1 ? "s" : ""}` : "done"));
	};
	faux.setResponses([step]);
	return faux.provider;
}
