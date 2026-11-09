import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { type ConversationId, type Harness, LiveDoc } from "@earendil-works/pi-durable";

// Substrate has no idle policy: pause once no run has been live for `afterMs`.
export async function pauseWhenIdle(harness: Harness, id: ConversationId, afterMs: number, pause: (stillIdle: () => boolean) => Promise<void>): Promise<void> {
	const watch = await harness.watchDoc(LiveDoc, id, context);
	if (!watch) return;
	let running = Boolean(watch.value.run);
	let timer: NodeJS.Timeout | undefined;
	const arm = () => {
		clearTimeout(timer);
		timer = running ? undefined : setTimeout(() => pause(() => !running).catch((e) => console.error(`pause after idle: ${e.message}`)), afterMs);
	};
	arm();
	watch.start(async (call) => {
		if (call.kind !== "value") return;
		running = Boolean(call.value.run);
		arm();
	});
}
