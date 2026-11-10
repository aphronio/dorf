export type Repo = { name: string; private: boolean };

export const REPO = /^[\w.-]+\/[\w.-]+$/;

// The deployment's GitHub token, which the egress gateway also gives the Sandboxes.
export class GitHub {
	readonly #basic: string;

	constructor(basic: string) {
		this.#basic = basic;
	}

	async repos(): Promise<Repo[]> {
		const res = await fetch("https://api.github.com/user/repos?per_page=100&sort=pushed", {
			headers: { authorization: `Basic ${this.#basic}`, accept: "application/vnd.github+json", "x-github-api-version": "2022-11-28" },
		});
		if (!res.ok) throw new Error(`GitHub repositories: ${res.status} ${await res.text()}`);
		return ((await res.json()) as { full_name: string; private: boolean }[]).map((r) => ({ name: r.full_name, private: r.private }));
	}
}
