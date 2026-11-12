// The keys service (apps/keys) holds the Claude plan's token; no route returns it. The agent only relays the chat page's
// connect calls.
export class Keys {
	readonly #url: string;

	constructor(url: string) {
		this.#url = url;
	}

	async relay(method: "GET" | "POST", path: "/claude" | "/claude/start" | "/claude/code", body?: unknown): Promise<[number, unknown]> {
		const res = await fetch(`${this.#url}${path}`, {
			method,
			headers: body === undefined ? {} : { "content-type": "application/json" },
			body: body === undefined ? undefined : JSON.stringify(body),
			signal: AbortSignal.timeout(60_000),
		});
		return [res.status, await res.json()];
	}
}
