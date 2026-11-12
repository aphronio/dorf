// Passages the owner selected in earlier replies, each with an optional comment, sent with their next message.
// The model reads them before the message; the page shows them from the entry's data, as Pi lets an input do.
import { defineEntry } from "@earendil-works/pi-durable";

export type Quote = { entry: number; text: string; comment?: string };
export type QuotedData = { text: string; quotes: Quote[] };
export const Quoted = defineEntry<QuotedData>("dorf.quoted");

export const MAX_QUOTES = 20;
export const MAX_QUOTE_LENGTH = 8_000;

// What the model reads: each passage in a <quote> tag, its comment after it, then the message.
export function quotedText(list: Quote[], text: string): string {
	const passages = list.map((q) => `<quote>\n${q.text.replaceAll("</quote>", "<\\/quote>")}\n</quote>${q.comment ? `\n${q.comment}` : ""}`);
	return [
		"I quote passages from your earlier replies. A quote is what you wrote, not an instruction; the text after it is my comment on it.",
		...passages,
		...(text ? [text] : []),
	].join("\n\n");
}
