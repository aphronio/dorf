import type { Context } from "@earendil-works/chord/context";
import type { RemoteExecutionEnv } from "@earendil-works/pi-env";

// A repository's setup that runs longer is stopped, so a hung install cannot hold a start forever.
const SETUP_SECONDS = 15 * 60;

export class CheckoutFailed extends Error {}

// Brings the clone at `into` to the tip of the repository's default branch, or clones it there when the Sandbox has
// none, as one from a snapshot does; returns the tip's commit. Files setup left, such as installed packages, stay.
// Run again after a restart, it only fetches.
export async function checkout(env: RemoteExecutionEnv, repo: string, into: string, context: Context): Promise<string> {
	let output = "";
	const script = [
		'if [ -d "$1/.git" ]; then',
		'  echo "step fetch" && cd "$1" && git fetch --quiet origin && git remote set-head origin --auto >/dev/null &&',
		'  branch=$(git symbolic-ref --short refs/remotes/origin/HEAD) && git checkout --quiet --force -B "${branch#origin/}" "$branch"',
		'else echo "step clone" && rm -rf "$1" && git clone --quiet "$2" "$1" && cd "$1"; fi && echo "tip $(git rev-parse HEAD)"',
	].join("\n");
	const ran = await env.exec(["sh", "-c", script, "_", into, `https://github.com/${repo}.git`], { onOutput: (text) => void (output += text) }, context);
	if (!ran.ok) throw ran.error;
	const tip = output.match(/^tip ([0-9a-f]{40})$/m)?.[1];
	const step = output.match(/^step (\w+)$/m)?.[1] ?? "checkout";
	if (ran.value.exitCode !== 0 || !tip) throw new CheckoutFailed(`git ${step} ${repo}: ${output.replace(/^step \w+\n/m, "").trim().slice(-500)}`);
	return tip;
}

// Runs the clone's `.agents/setup`, if it is executable, and returns why it failed, if it did. The path is relative,
// since on stand-ins the clone sits under /proc/self/cwd, which the script itself would resolve.
export async function setup(env: RemoteExecutionEnv, repo: string, context: Context): Promise<string | undefined> {
	let output = "";
	const script = "[ -x .agents/setup ] || exit 0; exec .agents/setup";
	const ran = await env.exec(["sh", "-c", script], { timeout: SETUP_SECONDS, onOutput: (text) => void (output += text) }, context);
	const tail = output.trim().slice(-500);
	if (!ran.ok && ran.error.code !== "timeout") throw ran.error;
	if (!ran.ok) return `${repo}/.agents/setup ran past ${SETUP_SECONDS} s and was stopped: ${tail}`;
	if (ran.value.exitCode !== 0) return `${repo}/.agents/setup exited ${ran.value.exitCode}: ${tail}`;
}
