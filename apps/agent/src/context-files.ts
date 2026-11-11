import { posix } from "node:path";
import { defineExtension, section } from "@earendil-works/pi-durable";

const NAMES = ["AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"];

// The working directory and each directory above it, nearest last, as Pi's coding agent reads them.
export function ancestors(cwd: string): string[] {
	const dirs = [cwd];
	while (dirs[0] !== "/") dirs.unshift(posix.dirname(dirs[0]!));
	return dirs;
}

// The ContextFiles extension of Pi Durable's spec, with Pi's coding agent's discovery and text, read from the Thread's machine.
export const ContextFiles = defineExtension({
	name: "context-files",
	sections: [
		section("project_context", async (input, context) => {
			const env = input.env;
			if (!env) return undefined;
			const files = await Promise.all(
				ancestors(env.cwd).map(async (dir) => {
					const listed = await env.listDir(dir, context);
					const names = new Set(listed.ok ? listed.value.filter((f) => f.kind !== "directory").map((f) => f.name) : []);
					const name = NAMES.find((n) => names.has(n));
					if (!name) return undefined;
					const path = posix.join(dir, name);
					const read = await env.readTextFile(path, context);
					return read.ok ? `<project_instructions path="${path}">\n${read.value}\n</project_instructions>` : undefined;
				}),
			);
			const found = files.filter((f) => f !== undefined);
			return found.length ? ["Project-specific instructions and guidelines:", ...found].join("\n\n") : undefined;
		}),
	],
});
