import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { type ConversationId, LiveDoc } from "@earendil-works/pi-durable";
import type { Threads } from "./threads.ts";

// Substrate has no idle policy: pause a Thread's Sandbox once no run has been live for `afterMs`.
export async function pauseWhenIdle(threads: Threads, id: ConversationId, afterMs: number): Promise<void> {
	const watch = await threads.harness.watchDoc(LiveDoc, id, context);
	if (!watch) return;
	let timer: NodeJS.Timeout | undefined;
	watch.start(async (call) => {
		if (call.kind !== "value") return;
		clearTimeout(timer);
		timer = call.value.run ? undefined : setTimeout(() => pause(threads, id), afterMs);
	});
}

async function pause(threads: Threads, id: ConversationId): Promise<void> {
	const name = await threads.sandbox(id);
	if (!name) return;
	try {
		if ((await threads.sandboxes.state(name)) === "ACTOR_STATE_RUNNING") await threads.sandboxes.pause(name);
	} catch (e) {
		console.error(`pause ${name}:`, (e as Error).message);
	}
}
