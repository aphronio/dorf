import type { Sandboxes } from "./sandbox.ts";

// Substrate SIGTERMs the Actors on a worker that is going away and leaves suspending them to their
// driver, within 30 minutes; an Actor still running then crashes. The agent is that driver.
export function drain(sandboxes: Sandboxes, everyMs = 5_000): void {
	const draining = new Set<string>();
	const tick = async () => {
		try {
			for (const name of await sandboxes.evicted()) {
				if (draining.has(name)) continue;
				draining.add(name);
				console.log(`drain: pausing ${name}`);
				sandboxes
					.pause(name, "drain")
					.catch((e) => console.error(`drain ${name}: ${e.message}`))
					.finally(() => draining.delete(name));
			}
		} catch (e) {
			console.error(`drain: ${(e as Error).message}`);
		}
		setTimeout(tick, everyMs);
	};
	void tick();
}
