import { posix } from "node:path";
import { defineExtension, section } from "@earendil-works/pi-durable";

const NAMES = ["AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"];

// The working directory and each directory above it, nearest last, as Pi's coding agent reads them.
export function ancestors(cwd: string): string[] {
	const dirs = [cwd];
	while (dirs[0] !== "/") dirs.unshift(posix.dirname(dirs[0]!));
	return dirs;
}

// The project's AGENTS.md files in the Thread's machine: one per directory from the root down to the working directory.
export const ContextFiles = defineExtension({
	name: "context-files",
	sections: [
		section("project_instructions", async (input, context) => {
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
					return read.ok ? `<file path="${path}">\n${read.value.trim()}\n</file>` : undefined;
				}),
			);
			const found = files.filter((f) => f !== undefined);
			return found.length ? `Project instructions, followed by the agent working here:\n\n${found.join("\n\n")}` : undefined;
		}),
	],
});
