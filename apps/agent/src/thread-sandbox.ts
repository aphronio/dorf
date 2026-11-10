import { defineDoc } from "@earendil-works/pi-durable";

// The Sandbox a conversation's tools run in: a Thread's own, and its subagents share it.
export const ThreadSandbox = defineDoc<{ name?: string }>({
	kind: "dorf.sandbox",
	version: 1,
	scope: "conversation",
	history: "latest",
	fork: "initial",
	initial: () => ({}),
	checkpointWhen: () => false,
});
