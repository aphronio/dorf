import { BACKGROUND_CONTEXT as context } from "@earendil-works/chord/context";
import { type ConversationId, type Harness, LiveDoc } from "@earendil-works/pi-durable";

// Substrate has no idle policy: pause once no run has been live for `afterMs`, and try again after as long when
// `pause` finds the Sandbox still busy. `now()` pauses as soon as no run is live, until the next run starts.
export async function pauseWhenIdle(
	harness: Harness,
	id: ConversationId,
	afterMs: number,
	pause: (stillIdle: () => boolean) => Promise<unknown | "busy">,
	onChange: (running: boolean) => void,
): Promise<{ now(): void } | undefined> {
	const watch = await harness.watchDoc(LiveDoc, id, context);
	if (!watch) return undefined;
	let running = Boolean(watch.value.run);
	let timer: NodeJS.Timeout | undefined;
	let reported: boolean | undefined;
	let soon = false;
	const arm = () => {
		if (running !== reported) onChange((reported = running));
		clearTimeout(timer);
		timer = running
			? undefined
			: setTimeout(
					() =>
						pause(() => !running).then(
							(result) => result === "busy" && !running && arm(),
							(e) => console.error(`pause after idle: ${e.message}`),
						),
					soon ? 0 : afterMs,
				);
	};
	arm();
	watch.start(async (call) => {
		if (call.kind !== "value") return;
		running = Boolean(call.value.run);
		if (running) soon = false;
		arm();
	});
	return {
		now() {
			soon = true;
			arm();
		},
	};
}
