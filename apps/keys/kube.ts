import { readFileSync } from "node:fs";

const KUBE = "/var/run/secrets/kubernetes.io/serviceaccount";

// A refusal the owner can act on; any other error is logged and reported as a failure of this service.
export class Refused extends Error {}

// Reads a Secret, or merges `patch` into it, asking up to three times. The service's Roles name each Secret it may touch.
export async function secret(namespace: string, name: string, patch?: object): Promise<any> {
	for (let attempt = 1; ; attempt++) {
		try {
			return await once(namespace, name, patch);
		} catch (e) {
			if (attempt === 3) throw e;
			await new Promise((r) => setTimeout(r, attempt * 1000));
		}
	}
}

async function once(namespace: string, name: string, patch?: object): Promise<any> {
	const res = await fetch(`https://kubernetes.default.svc/api/v1/namespaces/${namespace}/secrets/${name}`, {
		method: patch ? "PATCH" : "GET",
		headers: { authorization: `Bearer ${readFileSync(`${KUBE}/token`, "utf8")}`, "content-type": "application/merge-patch+json" },
		body: patch && JSON.stringify(patch),
	});
	if (!res.ok) throw new Error(`Secret ${namespace}/${name}: ${patch ? "PATCH" : "GET"} ${res.status}`);
	return res.json();
}

export const base64 = (text: string) => Buffer.from(text).toString("base64");

// The broker's credential injector reads an SDS file holding the whole Authorization header, and reloads it when the
// Secret changes.
export const sds = (name: string, header: string) =>
	base64(JSON.stringify({ resources: [{ "@type": "type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.Secret", name, generic_secret: { secret: { inline_string: header } } }] }));

// Each key's changes read what is stored and replace it, so they run one at a time.
export function oneAtATime(): <T>(run: () => Promise<T>) => Promise<T> {
	let turn: Promise<unknown> = Promise.resolve();
	return (run) => {
		const next = turn.then(run, run);
		turn = next.catch(() => {});
		return next;
	};
}
