// A scripted model for tests and keyless runs: every line of the newest user message that starts
// with `$ ` becomes one bash call, in order (`$[5] ` sets a 5 s timeout); then it answers "done".
// It reads only the transcript, so it gives the same next step after a restart.
import { fauxAssistantMessage, fauxProvider, fauxToolCall, type FauxResponseFactory } from "@earendil-works/pi-ai/providers/faux";

export const FAUX_MODEL = { provider: "faux", modelId: "faux" };

export function scriptedModel() {
	const faux = fauxProvider({ provider: FAUX_MODEL.provider, models: [{ id: FAUX_MODEL.modelId }] });
	const step: FauxResponseFactory = (transcript) => {
		faux.appendResponses([step]);
		const messages = transcript.messages;
		const lastUser = messages.findLastIndex((m) => m.role === "user");
		const content = messages[lastUser]?.content;
		const text = typeof content === "string" ? content : (content ?? []).map((c) => (c.type === "text" ? c.text : "")).join("");
		const commands = text.split("\n").flatMap((l) => {
			const call = l.match(/^\$(?:\[(\d+)\])? (.*)$/);
			return call ? [{ command: call[2], ...(call[1] ? { timeout: Number(call[1]) } : {}) }] : [];
		});
		const done = messages.slice(lastUser + 1).filter((m) => m.role === "toolResult").length;
		return done < commands.length
			? fauxAssistantMessage(fauxToolCall("bash", commands[done]), { stopReason: "toolUse" })
			: fauxAssistantMessage("done");
	};
	faux.setResponses([step]);
	return faux.provider;
}
