import { defineDoc } from "@earendil-works/pi-durable";

// The Sandbox a conversation's tools run in and the repository cloned in it: a Thread's own, and its subagents share
// it.
export const ThreadSandbox = defineDoc<{ name?: string; repo?: string }>({
	kind: "dorf.sandbox",
	version: 1,
	scope: "conversation",
	history: "latest",
	fork: "initial",
	initial: () => ({}),
	checkpointWhen: () => false,
});
