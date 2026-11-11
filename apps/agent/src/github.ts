export type Repo = { name: string; private: boolean };

export const REPO = /^[\w.-]+\/[\w.-]+$/;

// The broker adds the deployment's GitHub token, which the egress gateway also gives the Sandboxes.
export class GitHub {
	readonly #broker: string;

	constructor(broker: string) {
		this.#broker = broker;
	}

	async repos(): Promise<Repo[]> {
		const res = await fetch(`${this.#broker}/user/repos?per_page=100&sort=pushed`, {
			headers: { accept: "application/vnd.github+json", "x-github-api-version": "2022-11-28" },
		});
		if (!res.ok) throw new Error(`GitHub repositories: ${res.status} ${await res.text()}`);
		return ((await res.json()) as { full_name: string; private: boolean }[]).map((r) => ({ name: r.full_name, private: r.private }));
	}
}
