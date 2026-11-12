import { base64, oneAtATime, sds, secret } from "./kube.ts";

// GitHub, through Dorf's GitHub App and its device flow: the owner approves a short code on github.com, and the service
// keeps the refresh token in dorf-broker/github-login, which nothing else reads. It refreshes the 8-hour user token
// before it runs out and writes it where the egress gateway gives it to git and gh, and where the broker reads it. The
// device flow needs only the app's public client id, so no app secret or private key is kept anywhere.

// Dorf's own app; a deployment may name its own instead.
const CLIENT = process.env.DORF_GITHUB_CLIENT || "Iv23liysCcb0EV1pMrSD";
const APP = process.env.DORF_GITHUB_APP || "dorf-agent";
const GITHUB = "https://github.com";
const LOGIN = "github-login";
const CHECK_MS = 10 * 60_000;
const EARLY_MS = 60 * 60_000;

type Answer = { access_token?: string; expires_in?: number; refresh_token?: string; error?: string; interval?: number };
type Flow = { code: string; url: string; device: string; until: number; interval: number; stopped: boolean };
type Stored = { connected: string; login: string; refresh: string; expires: number };

const inTurn = oneAtATime();
let flow: Flow | undefined;
let failed: string | undefined;

async function post(path: string, form: Record<string, string>): Promise<any> {
	const res = await fetch(`${GITHUB}${path}`, {
		method: "POST",
		headers: { accept: "application/json", "content-type": "application/x-www-form-urlencoded" },
		body: new URLSearchParams({ client_id: CLIENT, ...form }),
		redirect: "error",
		signal: AbortSignal.timeout(15_000),
	});
	if (!res.ok) throw new Error(`GitHub ${path} answered ${res.status}`);
	return res.json();
}

async function login(token: string): Promise<string> {
	const res = await fetch("https://api.github.com/user", {
		headers: { accept: "application/vnd.github+json", authorization: `Bearer ${token}` },
		redirect: "error",
		signal: AbortSignal.timeout(15_000),
	});
	if (!res.ok) throw new Error(`GitHub /user answered ${res.status}`);
	return ((await res.json()) as { login: string }).login;
}

async function stored(): Promise<Stored | undefined> {
	const s = await secret("dorf-broker", LOGIN);
	const field = (k: string) => Buffer.from(s.data?.[k] ?? "", "base64").toString();
	const connected = s.metadata?.annotations?.["dorf.dev/connected"];
	return connected ? { connected, login: field("login"), refresh: field("refresh"), expires: Number(field("expires")) } : undefined;
}

// The egress gateway reads dorf-secrets/github on each request; the broker reloads its SDS file. No token is "none".
async function publish(token: string | null): Promise<void> {
	const basic = base64(`x-access-token:${token ?? "none"}`);
	await secret("dorf-secrets", "github", { data: { basic: base64(basic) } });
	await secret("dorf-broker", "github", { data: { "sds.yaml": sds("github", `Basic ${basic}`) } });
}

// GitHub replaces the refresh token on every refresh, so it is kept first. The expiry is written last: if publishing
// fails, the old expiry stands and the next check refreshes again.
async function keep(answer: Answer, who: string, connected: string): Promise<void> {
	const expires = answer.expires_in ? Date.now() + answer.expires_in * 1000 : Number.MAX_SAFE_INTEGER;
	await secret("dorf-broker", LOGIN, { metadata: { annotations: { "dorf.dev/connected": connected } }, data: { login: base64(who), refresh: base64(answer.refresh_token ?? "") } });
	await publish(answer.access_token!);
	await secret("dorf-broker", LOGIN, { data: { expires: base64(String(expires)) } });
	failed = undefined;
}

async function forget(): Promise<void> {
	await secret("dorf-broker", LOGIN, { metadata: { annotations: { "dorf.dev/connected": null } }, data: { login: null, refresh: null, expires: null } });
	await publish(null);
}

async function poll(f: Flow): Promise<void> {
	while (!f.stopped && Date.now() < f.until) {
		await new Promise((r) => setTimeout(r, f.interval * 1000));
		if (f.stopped) return;
		// A dropped poll is asked again; the code stays good until it expires.
		const answer: Answer = await post("/login/oauth/access_token", { device_code: f.device, grant_type: "urn:ietf:params:oauth:grant-type:device_code" }).catch(() => ({
			error: "authorization_pending",
		}));
		if (f.stopped) return;
		if (answer.error === "authorization_pending") continue;
		if (answer.error === "slow_down") {
			f.interval = answer.interval ?? f.interval + 5;
			continue;
		}
		if (flow === f) flow = undefined;
		if (!answer.access_token) {
			failed = answer.error === "access_denied" ? "GitHub sign-in was declined" : `GitHub ended the sign-in (${answer.error})`;
			return;
		}
		try {
			const who = await login(answer.access_token);
			if (f.stopped) return;
			await inTurn(() => keep(answer, who, new Date().toISOString()));
		} catch (e) {
			failed = "Dorf could not keep the GitHub sign-in; connect again";
			console.error((e as Error).message);
		}
		return;
	}
	if (flow === f) {
		flow = undefined;
		failed = "The code expired; connect again";
	}
}

async function refresh(): Promise<void> {
	const s = await stored();
	if (!s?.refresh || s.expires - Date.now() > EARLY_MS) return;
	const answer: Answer = await post("/login/oauth/access_token", { grant_type: "refresh_token", refresh_token: s.refresh });
	if (answer.error === "bad_refresh_token") {
		await forget();
		failed = "GitHub ended Dorf's sign-in; connect again";
		return;
	}
	if (!answer.access_token) throw new Error(`GitHub did not refresh the sign-in (${answer.error})`);
	await keep(answer, s.login, s.connected);
}

const check = () => void inTurn(refresh).catch((e) => console.error((e as Error).message));
check();
setInterval(check, CHECK_MS);

export async function status() {
	const s = await stored();
	return {
		connected: s?.connected ?? null,
		login: s?.login,
		pending: flow && { code: flow.code, url: flow.url },
		error: failed,
		repositories: `https://github.com/apps/${APP}/installations/new`,
	};
}

export async function start() {
	const d = await post("/login/device/code", {});
	if (!d.device_code) throw new Error(`GitHub gave no device code (${d.error})`);
	if (flow) flow.stopped = true;
	failed = undefined;
	const f: Flow = { code: d.user_code, url: d.verification_uri, device: d.device_code, until: Date.now() + d.expires_in * 1000, interval: d.interval, stopped: false };
	flow = f;
	void poll(f);
	return status();
}

// Cancels a waiting sign-in, and forgets one Dorf holds. GitHub revokes a user token only for the app's secret, which
// Dorf does not keep; the token Dorf forgets stops working within 8 hours, or at once when the owner revokes the app on
// github.com. A token Dorf did not write, such as one set at deploy time, stays.
export async function disconnect() {
	if (flow) flow.stopped = true;
	flow = undefined;
	failed = undefined;
	await inTurn(async () => {
		if (await stored()) await forget();
	});
	return status();
}
