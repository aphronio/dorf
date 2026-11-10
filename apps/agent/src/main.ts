import { mkdirSync } from "node:fs";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { builtinModels } from "@earendil-works/pi-ai/providers/all";
import { createRegistry, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { CodingTools } from "@earendil-works/pi-durable/tools";
import { bundledClaude, ClaudeLogin } from "./claude-login.ts";
import { ClaudePlan } from "./claude-plan.ts";
import { FAUX_MODEL, scriptedModel } from "./faux.ts";
import { serve } from "./http.ts";
import { Sandboxes } from "./sandbox.ts";
import { drain } from "./drain.ts";
import { SandboxJobs } from "./jobs.ts";
import { sandboxEnv, Threads } from "./threads.ts";

const DATA = process.env.DORF_DATA ?? "data";
const PORT = Number(process.env.DORF_PORT ?? 8091);
const IDLE_MS = Number(process.env.DORF_IDLE_MS ?? 5 * 60_000);
const WORKSPACE = "/workspace";
const MODEL = process.env.DORF_MODEL ?? "faux";
const OWNER = process.env.DORF_OWNER;
if (!OWNER) throw new Error("set DORF_OWNER to the Tailscale login allowed to use this agent");
const CLAUDE = process.env.DORF_CLAUDE_BIN ?? bundledClaude();

const models = builtinModels();
const slash = MODEL.indexOf("/");
if (MODEL !== "faux" && slash < 1) throw new Error(`DORF_MODEL must be "faux" or "<provider>/<model>", not "${MODEL}"`);
const model = MODEL === "faux" ? FAUX_MODEL : { provider: MODEL.slice(0, slash), modelId: MODEL.slice(slash + 1) };
if (MODEL === "faux") models.setProvider(scriptedModel());
mkdirSync(DATA, { recursive: true });
const claudePlan = new ClaudePlan(`${DATA}/claude`, CLAUDE);
models.setProvider(claudePlan.provider);

const registry = createRegistry();
const sandboxes = new Sandboxes(process.env.DORF_GATE ?? "http://127.0.0.1:8080", process.env.DORF_TOKEN_FILE ?? "token");
registry.install(CodingTools);
registry.install(SandboxJobs);
const harness = await Harness.open(
	await openNodeSqliteStorage(`${DATA}/agent.sqlite`),
	{
		models,
		registry,
		identity: { attribution: null },
		env: sandboxEnv(sandboxes, WORKSPACE),
		onReport: (report) => console.error(`pi ${report.kind}:`, report.error),
	},
	context,
);
harness.closed.then((end) => process.exit(end.kind === "failed" ? 1 : 0));
process.on("SIGTERM", () => harness.close(context));
harness.resume();

const threads = new Threads(
	harness,
	sandboxes,
	{
		model,
		cwd: WORKSPACE,
		instructions: `You work in a Linux machine of your own, with internet access. The workspace is ${WORKSPACE}. Git over HTTPS to github.com is already authenticated: clone and push the owner's repositories without asking for a token. Keep answers short.`,
	},
	IDLE_MS,
	claudePlan,
);
await threads.open();
drain(sandboxes);
serve(threads, new ClaudeLogin(CLAUDE), OWNER, PORT);
console.log(`dorf agent on 127.0.0.1:${PORT}, model ${model.provider}/${model.modelId}`);
