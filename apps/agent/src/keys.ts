// The keys service (apps/keys) holds the Claude plan's and GitHub's tokens; no route returns one. The agent only relays
// the chat page's connect calls.
export class Keys {
	readonly #url: string;

	constructor(url: string) {
		this.#url = url;
	}

	// oxlint-disable-next-line anti-slop/no-unknown-parameters -- the page's body passes through unread; the keys service parses it
	async relay(method: "GET" | "POST", path: `/${"claude" | "github"}${"" | "/start" | "/code" | "/disconnect"}`, body?: unknown): Promise<[number, unknown]> {
		const res = await fetch(`${this.#url}${path}`, {
			method,
			headers: body === undefined ? {} : { "content-type": "application/json" },
			// oxlint-disable-next-line unicorn/no-invalid-fetch-options -- the body is undefined on every GET
			body: body === undefined ? undefined : JSON.stringify(body),
			signal: AbortSignal.timeout(60_000),
		});
		return [res.status, await res.json()];
	}
}
