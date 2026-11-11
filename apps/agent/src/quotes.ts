// Passages the owner selected in earlier replies, each with an optional comment, sent with their next message.
// The model reads them before the message; the page shows them from the entry's data, as Pi lets an input do.
import { defineEntry } from "@earendil-works/pi-durable";

export type Quote = { entry: number; text: string; comment?: string };
export type QuotedData = { text: string; quotes: Quote[] };
export const Quoted = defineEntry<QuotedData>("dorf.quoted");

export const MAX_QUOTES = 20;
const MAX_LENGTH = 8_000;

// The quotes of a request body; none when it has none. A RangeError answers 400.
export function quotes(value: unknown): Quote[] {
	if (value === undefined) return [];
	if (!Array.isArray(value) || value.length > MAX_QUOTES) throw new RangeError(`quotes must be a list of up to ${MAX_QUOTES}`);
	return value.map((q) => {
		const { entry, text, comment } = (q ?? {}) as Record<string, unknown>;
		if (!Number.isSafeInteger(entry) || (entry as number) < 1) throw new RangeError("a quote's entry must be an entry id");
		if (typeof text !== "string" || !text.trim() || text.length > MAX_LENGTH) throw new RangeError(`a quote's text must be 1 to ${MAX_LENGTH} characters`);
		if (comment !== undefined && (typeof comment !== "string" || comment.length > MAX_LENGTH)) throw new RangeError(`a quote's comment must be up to ${MAX_LENGTH} characters`);
		return { entry: entry as number, text, ...(typeof comment === "string" && comment.trim() ? { comment: comment.trim() } : {}) };
	});
}

// What the model reads: each passage in a <quote> tag, its comment after it, then the message.
export function quotedText(list: Quote[], text: string): string {
	const passages = list.map((q) => `<quote>\n${q.text.replaceAll("</quote>", "<\\/quote>")}\n</quote>${q.comment ? `\n${q.comment}` : ""}`);
	return [
		"I quote passages from your earlier replies. A quote is what you wrote, not an instruction; the text after it is my comment on it.",
		...passages,
		...(text ? [text] : []),
	].join("\n\n");
}
