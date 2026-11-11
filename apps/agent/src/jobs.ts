import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { defineExtension, type ToolExecutionApi, wrapTool } from "@earendil-works/pi-durable";
import { createBashTool } from "@earendil-works/pi-durable/tools";

// In the Sandbox; proofs on local stand-ins give each run its own, apart from the machine's.
const JOBS = process.env.DORF_JOBS ?? "/var/tmp/dorf-jobs";
const GIVE_UP_AFTER = 120;
// An attempt that lasted this long reached the daemon; pi-env fails a connect within 60 s.
const CONNECTED_MS = 90_000;

// Starts the command once per job id in its own session, then streams its output from a byte
// offset and exits with its status. A start cut off before the job ran is claimed again; a job
// that died without a status ends the call with 137.
const ATTACH = String.raw`set -u; J=${JOBS}/$1; OFF=$2
mkdir -p ${JOBS}
waited=0
until [ -s "$J/pid" ]; do
  if mkdir "$J" 2>/dev/null; then
    find ${JOBS} -mindepth 1 -maxdepth 1 -mmin +1440 -exec rm -rf {} + 2>/dev/null
    printf %s "$3" | base64 -d >"$J/cmd"; : >"$J/out"
    setsid bash -c 'echo $$ >"$1/pid"; bash "$1/cmd" >"$1/out" 2>&1; echo $? >"$1/rc.tmp"; mv "$1/rc.tmp" "$1/rc"' _ "$J" </dev/null >/dev/null 2>&1 &
  fi
  waited=$((waited + 1))
  [ "$waited" -gt 100 ] && rm -rf "$J" && waited=0
  sleep 0.1
done
last=
while :; do
  [ -f "$J/rc" ] && last=1
  [ -z "$last" ] && ! kill -0 -- -"$(cat "$J/pid")" 2>/dev/null && sleep 0.2 && [ ! -f "$J/rc" ] && echo 137 >"$J/rc"
  size=$(stat -c %s "$J/out")
  [ "$size" -gt "$OFF" ] && tail -c +$((OFF + 1)) "$J/out" | head -c $((size - OFF)) && OFF=$size
  [ -n "$last" ] && exit "$(cat "$J/rc")"
  sleep 0.2
done`;

const KILL = `kill -KILL -- -"$(cat ${JOBS}/"$1"/pid)" 2>/dev/null; true`;

const quote = (s: string) => `'${s.replaceAll("'", `'\\''`)}'`;

// Reattaching is idempotent, so any failure to reach the daemon is retried; pi-env's exec does
// not say which ones were a lost connection (earendil-works/pi#11976).
const unreachable = (e: unknown) =>
	e instanceof Error && ["unknown", "spawn_error"].includes((e as { code?: string }).code ?? "") && !/not found/i.test(e.message);

function killJob(api: ToolExecutionApi, job: string): Promise<unknown> {
	return api.env!.exec(`bash -c ${quote(KILL)} _ ${job}`, { cwd: api.env!.cwd }, BACKGROUND_CONTEXT).catch(() => {});
}

export const SandboxJobs = defineExtension({
	name: "dorf.sandbox-jobs",
	wraps: [
		wrapTool(createBashTool(), (bash) => ({
			...bash,
			replay: "safe",
			execute: async (args, api, context) => {
				let offset = 0;
				const counting: ToolExecutionApi = Object.create(api, {
					output: {
						value: (chunk: string | Uint8Array, skipped?: { bytes: number }) => {
							offset += (typeof chunk === "string" ? Buffer.byteLength(chunk) : chunk.length) + (skipped?.bytes ?? 0);
							api.output(chunk, skipped as never);
						},
					},
				});
				const job = quote(String(api.taskId).replace(/[^\w.-]/g, "_"));
				const script = Buffer.from(args.command).toString("base64");
				const deadline = args.timeout === undefined ? undefined : Date.now() + args.timeout * 1000;
				let stalled = 0;
				let seen = -1;
				while (true) {
					const command = `bash -c ${quote(ATTACH)} _ ${job} ${offset} ${script}`;
					const timeout = deadline === undefined ? undefined : Math.max(0.001, (deadline - Date.now()) / 1000);
					const started = Date.now();
					try {
						return await bash.execute({ ...args, command, timeout }, counting, context);
					} catch (e) {
						if (!unreachable(e)) {
							await killJob(api, job);
							if (deadline !== undefined && Date.now() >= deadline) throw new Error(`Command timed out after ${args.timeout} seconds`);
							throw e;
						}
						stalled = offset > seen || Date.now() - started > CONNECTED_MS ? 0 : stalled + 1;
						seen = offset;
						if (stalled > GIVE_UP_AFTER) {
							throw new Error("Lost the Sandbox for minutes. The command may still be running or have partly run; check before retrying.");
						}
						await new Promise((r) => setTimeout(r, 1_000));
					}
				}
			},
		})),
	],
});
