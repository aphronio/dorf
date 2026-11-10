import { type ChildProcess, execFile, spawn } from "node:child_process";
import { createRequire } from "node:module";
import { claudeEnv } from "./claude-session.ts";

const LOGIN_TTL_MS = 10 * 60_000;
const LINK_WAIT_MS = 20_000;
const CODE_WAIT_MS = 30_000;

export function bundledClaude(): string {
	return createRequire(import.meta.url).resolve(`@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}/claude`);
}

export type ClaudeStatus = { loggedIn: boolean; plan: string | null; email: string | null };

export class LoginError extends Error {}

// Drives Claude Code's own `auth login`: the owner signs in on claude.com and pastes the code back.
export class ClaudeLogin {
	readonly #bin: string;
	#login?: { child: ChildProcess; done: Promise<unknown> };

	constructor(bin: string) {
		this.#bin = bin;
	}

	status(): Promise<ClaudeStatus> {
		return new Promise((resolve) =>
			execFile(this.#bin, ["auth", "status"], { env: claudeEnv() }, (_e, out) => {
				try {
					const s = JSON.parse(out);
					resolve({ loggedIn: Boolean(s.loggedIn), plan: s.subscriptionType ?? null, email: s.email ?? null });
				} catch {
					resolve({ loggedIn: false, plan: null, email: null });
				}
			}),
		);
	}

	start(): Promise<string> {
		this.#login?.child.kill();
		const child = spawn(this.#bin, ["auth", "login", "--claudeai"], { env: claudeEnv(), stdio: ["pipe", "pipe", "pipe"] });
		const expire = setTimeout(() => child.kill(), LOGIN_TTL_MS);
		const done = new Promise((resolve) => child.once("exit", (code) => (clearTimeout(expire), resolve(code))));
		this.#login = { child, done };
		return new Promise((resolve, reject) => {
			let out = "";
			const read = (chunk: Buffer) => {
				out += chunk;
				const link = out.match(/https:\/\/\S+oauth\/authorize\S+/)?.[0];
				if (link) resolve(link);
			};
			child.stdout.on("data", read);
			child.stderr.on("data", read);
			done.then(() => reject(new LoginError("Claude Code exited before printing a sign-in link")));
			setTimeout(() => reject(new LoginError("no sign-in link within 20 s")), LINK_WAIT_MS);
		});
	}

	async finish(code: unknown): Promise<ClaudeStatus> {
		const login = this.#login;
		if (!login) throw new LoginError("start the sign-in first");
		if (typeof code !== "string" || !/^[\w#.~-]{8,512}$/.test(code.trim())) throw new LoginError("that does not look like a sign-in code");
		login.child.stdin!.end(`${code.trim()}\n`);
		await Promise.race([login.done, new Promise((r) => setTimeout(r, CODE_WAIT_MS))]);
		login.child.kill();
		this.#login = undefined;
		return this.status();
	}
}
