import { type ConversationId, defineDoc } from "@earendil-works/pi-durable";

// `starting` until the Thread's first message is in, `waiting` meanwhile while no worker has room, since when and with
// which Threads holding the machines, and `failure` if it never got in. `working` while a run is live; `attention` once it ended, until the owner opens the Thread.
// `request` is the page's id for the draft that created it, so a repeat finds this Thread.
// `askedAt` is the owner's last message, `startedAt` and `endedAt` the newest run's.
export type ThreadSummary = {
	id: ConversationId;
	title: string;
	createdAt: string;
	repo?: string;
	archived?: true;
	starting?: true;
	waiting?: { since: string; busy: ConversationId[] };
	failure?: string;
	working?: true;
	attention?: "done" | "failed";
	request?: string;
	askedAt?: string;
	startedAt?: string;
	endedAt?: string;
};

export const ThreadList = defineDoc<{ items: ThreadSummary[] }>({
	kind: "dorf.threads",
	version: 1,
	scope: "session",
	initial: () => ({ items: [] }),
	checkpointWhen: () => false,
});
