import { mkdirSync } from "node:fs";
import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { builtinModels } from "@earendil-works/pi-ai/providers/all";
import { type ConversationId, createRegistry, Harness } from "@earendil-works/pi-durable";
import { openNodeSqliteStorage } from "@earendil-works/pi-durable/storage/sqlite/node";
import { CodingTools } from "@earendil-works/pi-durable/tools";
import { FAUX_MODEL, scriptedModel } from "./faux.ts";
import { serve } from "./http.ts";
import { pauseWhenIdle } from "./idle.ts";
import { Sandboxes } from "./sandbox.ts";
import { sandboxEnv, Threads, WORKSPACE } from "./threads.ts";

const DATA = process.env.DORF_DATA ?? "data";
const PORT = Number(process.env.DORF_PORT ?? 8091);
const IDLE_MS = Number(process.env.DORF_IDLE_MS ?? 5 * 60_000);
// `faux` or `<provider>/<model>`; a real provider reads its key from its usual environment variable.
const MODEL = process.env.DORF_MODEL ?? "faux";

const models = builtinModels();
const [provider, modelId] = MODEL === "faux" ? [FAUX_MODEL.provider, FAUX_MODEL.modelId] : MODEL.split("/", 2);
if (MODEL === "faux") models.setProvider(scriptedModel());

const registry = createRegistry();
registry.install(CodingTools);
const sandboxes = new Sandboxes();
mkdirSync(DATA, { recursive: true });
const harness = await Harness.open(
	await openNodeSqliteStorage(`${DATA}/agent.sqlite`),
	{ models, registry, identity: { attribution: null }, env: sandboxEnv(sandboxes) },
	context,
);
harness.resume();

const threads = new Threads(harness, sandboxes, {
	model: { provider, modelId },
	extensions: [CodingTools],
	cwd: WORKSPACE,
	instructions: `You work in a sandboxed Linux machine. The workspace is ${WORKSPACE}. Keep answers short.`,
});
const watchIdle = (id: ConversationId) => pauseWhenIdle(threads, id, IDLE_MS);
for (const thread of await threads.list()) await watchIdle(thread.id);

serve(threads, PORT, watchIdle);
console.log(`dorf agent on 127.0.0.1:${PORT}, model ${provider}/${modelId}`);
