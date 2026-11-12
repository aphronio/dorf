import { mkdirSync } from "node:fs";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { builtinModels } from "@earendil-works/pi-ai/providers/all";
import { createRegistry, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { createNodePhotonImages } from "@earendil-works/pi-durable/images/node";
import { createCodingTools } from "@earendil-works/pi-durable/tools";
import { ClaudePlan } from "./claude-plan.ts";
import { bundledClaude } from "./claude-session.ts";
import { FAUX_MODEL, scriptedModel } from "./faux.ts";
import { GitHub } from "./github.ts";
import { Keys } from "./keys.ts";
import { serve } from "./http.ts";
import { bounded } from "./images.ts";
import { Catalog } from "./models.ts";
import { ContextFiles } from "./context-files.ts";
import { prompt } from "./prompt.ts";
import { Sandboxes } from "./sandbox.ts";
import { Skills } from "./skills.ts";
import { subagents } from "./subagent.ts";
import { drain } from "./drain.ts";
import { SandboxJobs } from "./jobs.ts";
import { threadStart } from "./thread-start.ts";
import { sandboxEnv, Threads } from "./threads.ts";

const DATA = process.env.DORF_DATA ?? "data";
const PORT = Number(process.env.DORF_PORT ?? 8091);
const IDLE_MS = Number(process.env.DORF_IDLE_MS ?? 5 * 60_000);
const WORKSPACE = process.env.DORF_WORKSPACE ?? "/workspace";
const MODEL = process.env.DORF_MODEL ?? "faux";
const OWNER = process.env.DORF_OWNER;
if (!OWNER) throw new Error("set DORF_OWNER to the Tailscale login allowed to use this agent");
const CLAUDE = process.env.DORF_CLAUDE_BIN ?? bundledClaude();
const github = process.env.DORF_GITHUB && process.env.DORF_BROKER ? new GitHub(process.env.DORF_BROKER) : undefined;

const models = builtinModels();
const slash = MODEL.indexOf("/");
if (MODEL !== "faux" && slash < 1) throw new Error(`DORF_MODEL must be "faux" or "<provider>/<model>", not "${MODEL}"`);
const model = MODEL === "faux" ? FAUX_MODEL : { provider: MODEL.slice(0, slash), modelId: MODEL.slice(slash + 1) };
if (MODEL === "faux") models.setProvider(scriptedModel());
mkdirSync(DATA, { recursive: true });
const claudePlan = new ClaudePlan(`${DATA}/claude`, CLAUDE);
models.setProvider(claudePlan.provider);

const registry = createRegistry();
const sandboxes = new Sandboxes(process.env.DORF_GATE ?? "http://127.0.0.1:8080", process.env.DORF_TOKEN_FILE ?? "token", process.env.DORF_SANDBOX_PREFIX);
registry.install(prompt(WORKSPACE, github !== undefined));
registry.install(ContextFiles);
registry.install(Skills);
// One image policy: what `read` returns and what the owner attaches are both fitted to the Thread's model.
const images = bounded(await createNodePhotonImages());
registry.install(createCodingTools({ images }));
registry.install(SandboxJobs);
const catalog = new Catalog(models, model);
await catalog.refresh();
registry.install(subagents(catalog));
const start = threadStart(sandboxes);
registry.install(start.extension);
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
	start.task,
	{
		model,
		thinkingLevel: catalog.thinking(model),
		cwd: WORKSPACE,
	},
	IDLE_MS,
	claudePlan,
);
start.holders = () => threads.holders();
await threads.open();
drain(sandboxes);
serve(threads, catalog, github, process.env.DORF_KEYS ? new Keys(process.env.DORF_KEYS) : undefined, images, OWNER, PORT);
console.log(`dorf agent on 127.0.0.1:${PORT}, model ${model.provider}/${model.modelId}`);
