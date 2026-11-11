// Each `$ ` line of the newest user message becomes a bash call (`$[5] ` sets a 5 s timeout), and each `@ {...}` line
// a subagent call with those arguments; then "done". A message of just `?` gets the system prompt's sections back.
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
		const calls = text.split("\n").flatMap((l) => {
			const bash = l.match(/^\$(?:\[(\d+)\])? (.*)$/);
			if (bash) return [fauxToolCall("bash", { command: bash[2], ...(bash[1] ? { timeout: Number(bash[1]) } : {}) })];
			return l.startsWith("@ ") ? [fauxToolCall("subagent", JSON.parse(l.slice(2)))] : [];
		});
		const done = messages.slice(lastUser + 1).filter((m) => m.role === "toolResult").length;
		return done < calls.length
			? fauxAssistantMessage(calls[done], { stopReason: "toolUse" })
			: fauxAssistantMessage("done");
	};
	faux.setResponses([step]);
	return faux.provider;
}
