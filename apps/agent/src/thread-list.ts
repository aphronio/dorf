import { type ConversationId, defineDoc } from "@earendil-works/pi-durable";

// `starting` until the Thread's first message is in, `waiting` meanwhile while no worker has room, and `failure` if
// it never got in. `working` while a run is live; `attention` once it ended, until the owner opens the Thread.
// `request` is the page's id for the draft that created it, so a repeat finds this Thread.
export type ThreadSummary = {
	id: ConversationId;
	title: string;
	createdAt: string;
	repo?: string;
	archived?: true;
	starting?: true;
	waiting?: true;
	failure?: string;
	working?: true;
	attention?: "done" | "failed";
	request?: string;
};

export const ThreadList = defineDoc<{ items: ThreadSummary[] }>({
	kind: "dorf.threads",
	version: 1,
	scope: "session",
	initial: () => ({ items: [] }),
	checkpointWhen: () => false,
});
