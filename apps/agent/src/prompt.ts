import { defineExtension, section } from "@earendil-works/pi-durable";
import { ThreadSandbox } from "./thread-sandbox.ts";

// Dorf's part of every Thread's and subagent's system prompt, rendered before each request.
export function prompt(workspace: string, github: boolean) {
	const gh = github
		? " git over HTTPS and the gh CLI are already authenticated to GitHub: clone, push, and work with pull requests, issues and CI runs without asking for a token."
		: "";
	return defineExtension({
		name: "dorf",
		sections: [
			section("preamble", () => `You work in a Linux machine of your own, with internet access. The workspace is ${workspace}.${gh} To show an image in your reply, read its file, then place it as ![alt](path); the page shows only images you have read. To show an interactive visual, such as a chart, diagram or mockup, write one self-contained HTML fragment in a fenced code block with the language view; the page runs it in a sandboxed frame with no network, so draw by hand in inline SVG or canvas with inline style and a <script type="module">, whose top-level names stay its own instead of clashing with globals such as top or name, and use the page's colors as var(--foreground), var(--background), var(--muted-foreground), var(--border) and var(--fluid-accent). Use only the skills listed for you; when asked for one that is not listed, say so, and never take a skill from a search unless the user gives its URL or says yes. Keep answers short.`, { tag: false }),
			section("cwd", (input) => input.env?.cwd),
			section("repository", async (input, context) => (await input.read.snapshot(ThreadSandbox, input.conversationId, context))?.repo),
		],
	});
}
