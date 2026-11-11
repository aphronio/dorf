import { posix } from "node:path";
import { defineExtension, section } from "@earendil-works/pi-durable";
import { ancestors } from "./context-files.ts";

// A skill's name and description from its SKILL.md front matter; a folded or block description joins its lines.
export function frontMatter(text: string): Record<string, string> {
	const block = text.match(/^---\r?\n([\s\S]*?)\r?\n---/)?.[1] ?? "";
	const fields: Record<string, string> = {};
	let key: string | undefined;
	for (const line of block.split(/\r?\n/)) {
		const field = line.match(/^([A-Za-z][\w-]*):\s*(.*)$/);
		if (field) {
			key = field[1]!;
			fields[key] = /^[>|][-+]?$/.test(field[2]!) ? "" : field[2]!.replace(/^(["'])(.*)\1$/, "$2");
		} else if (key && /^\s+\S/.test(line)) fields[key] = `${fields[key]} ${line.trim()}`.trim();
	}
	return fields;
}

// Skills in the Thread's machine: each `.agents/skills/<name>/SKILL.md` from the root down to the working directory,
// listed the way Pi's coding agent lists them, for the model to read when a task matches.
export const Skills = defineExtension({
	name: "skills",
	sections: [
		section("skills", async (input, context) => {
			const env = input.env;
			if (!env) return undefined;
			const found = await Promise.all(
				ancestors(env.cwd).map(async (dir) => {
					const root = posix.join(dir, ".agents/skills");
					const listed = await env.listDir(root, context);
					if (!listed.ok) return [];
					const names = listed.value.filter((f) => f.kind !== "file").map((f) => f.name).sort();
					return Promise.all(
						names.map(async (name) => {
							const path = posix.join(root, name, "SKILL.md");
							const read = await env.readTextFile(path, context);
							const { description } = read.ok ? frontMatter(read.value) : {};
							return description ? { name, description, path } : undefined;
						}),
					);
				}),
			);
			const skills = found.flat().filter((s) => s !== undefined);
			if (!skills.length) return undefined;
			const listed = skills.map((s) => `  <skill>\n    <name>${s.name}</name>\n    <description>${s.description}</description>\n    <location>${s.path}</location>\n  </skill>`);
			return [
				"These skills give instructions for specific tasks. When a task matches a skill's description, read its SKILL.md first, and resolve its relative paths against the skill's directory.",
				`<available_skills>\n${listed.join("\n")}\n</available_skills>`,
			].join("\n");
		}),
	],
});
