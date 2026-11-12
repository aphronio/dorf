import { posix } from "node:path";
import { defineExtension, section } from "@earendil-works/pi-durable";
import { compile, str } from "@earendil-works/pi-schema";
import { parse } from "yaml";
import { ancestors } from "./context-files.ts";

type Skill = { name: string; description: string; location: string };
const isText = compile(str()).check;

type FrontMatter = { name?: string; description?: string; disabled?: boolean };

// A SKILL.md's front matter, parsed as Pi's coding agent parses it; a file without one has none.
function frontMatter(text: string): FrontMatter {
	const normalized = text.replace(/^﻿/, "").replace(/\r\n?/g, "\n");
	const end = normalized.startsWith("---") ? normalized.indexOf("\n---", 3) : -1;
	if (end === -1) return {};
	try {
		const { name, description, "disable-model-invocation": disabled } = parse(normalized.slice(4, end)) ?? {};
		return { name: isText(name) ? name : undefined, description: isText(description) ? description : undefined, disabled: disabled === true };
	} catch {
		return {};
	}
}

const escapeXml = (s: string) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&apos;");

// The Skills extension of Pi Durable's spec, with Pi's coding agent's text: each `.agents/skills/<dir>/SKILL.md` from the root down
// to the working directory of the Thread's machine, listed for the model to read when a task matches.
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
					const dirs = listed.value.filter((f) => f.kind !== "file").map((f) => f.name).sort();
					return Promise.all(
						dirs.map(async (dirName): Promise<Skill | undefined> => {
							const location = posix.join(root, dirName, "SKILL.md");
							const read = await env.readTextFile(location, context);
							const front = read.ok ? frontMatter(read.value) : {};
							if (!front.description?.trim() || front.disabled) return undefined;
							return { name: front.name || dirName, description: front.description, location };
						}),
					);
				}),
			);
			const skills = found.flat().filter((s) => s !== undefined);
			if (!skills.length) return undefined;
			return [
				"The following skills provide specialized instructions for specific tasks.",
				"Use the read tool to load a skill's file when the task matches its description.",
				"Resolve relative paths against the skill's directory (parent of SKILL.md). Use absolute paths in tool calls.",
				"",
				"<available_skills>",
				...skills.flatMap((s) => ["  <skill>", `    <name>${escapeXml(s.name)}</name>`, `    <description>${escapeXml(s.description)}</description>`, `    <location>${escapeXml(s.location)}</location>`, "  </skill>"]),
				"</available_skills>",
			].join("\n");
		}),
	],
});
