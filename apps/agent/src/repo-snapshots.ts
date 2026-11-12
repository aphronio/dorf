import { createHash } from "node:crypto";
import type { Context } from "@earendil-works/chord/context";
import { defineDoc, defineExtension, defineTask, ROOT_CONVERSATION_ID, type RunningTask, type TaskId, type TaskRuntime, type Tx } from "@earendil-works/pi-durable";
import { checkout, CheckoutFailed, setup } from "./repo-checkout.ts";
import { RETRY_ROOM_MS, type Sandboxes, SnapshotRefused } from "./sandbox.ts";

// Per repository, the snapshot new Threads start from, the commit its clone is at, and the task building the next.
export type RepoSnapshot = { snapshot?: string; sha?: string; building?: TaskId };

export const RepoSnapshots = defineDoc<{ repos: Record<string, RepoSnapshot> }>({
	kind: "dorf.repo-snapshots",
	version: 1,
	scope: "session",
	initial: () => ({ repos: {} }),
	checkpointWhen: () => false,
});

// How a Thread's Sandbox caught up: from the repository's snapshot or not, to which commit, in how long.
export type CatchUp = { fromSnapshot: boolean; tip: string; ms: number };

type BuildInput = { repo: string; workspace: string };
type BuildState = { phase: "build"; tries?: number } | { phase: "save"; name: string; sha: string; tries?: number } | { phase: "clean"; tries?: number };

// Gate errors, such as the gate restarting, are tried again this many times before the build gives up.
const TRIES = 5;

// Creates and wakes the Sandbox, from the snapshot if one is given. A snapshot Substrate refuses, or whose Sandbox
// fails its first wake, gives way to a plain Sandbox. `woke` is false while no worker has room.
export async function startFrom(sandboxes: Sandboxes, name: string, snapshot: string | undefined, makeRoom: boolean): Promise<{ woke: boolean; fromSnapshot: boolean }> {
	if (snapshot) {
		try {
			await sandboxes.create(name, snapshot);
			return { woke: await sandboxes.wake(name, makeRoom), fromSnapshot: true };
		} catch (e) {
			console.error(`${name} from snapshot ${snapshot}: ${(e as Error).message}; starting it without one`);
			if (!(e instanceof SnapshotRefused)) await sandboxes.delete(name);
		}
	}
	await sandboxes.create(name);
	return { woke: await sandboxes.wake(name, makeRoom), fromSnapshot: false };
}

// Deletes a repository's snapshots, named from `prefix`, that no Thread needs: those not ready, and those neither
// `keep` nor borrowed by a Sandbox that has not saved its own since it started from one.
export async function reap(sandboxes: Sandboxes, prefix: string, keep: string | undefined): Promise<string[]> {
	const gone = (await sandboxes.snapshots()).filter((s) => s.name.startsWith(prefix) && (!s.ready || (s.name !== keep && !s.borrowed)));
	for (const s of gone) await sandboxes.deleteSnapshot(s.name);
	return gone.map((s) => s.name);
}

// Saved Sandboxes that already hold a repository's clone with its `.agents/setup` run, as Amp's orbs do. A Thread
// that caught up slowly, or did not start from the snapshot, has a builder Sandbox clone, set up and save a newer one
// in the background. A builder never pauses a Thread to make room. Snapshots hold memory too, so a process setup
// leaves running is restored into every Thread.
export function repoSnapshots(sandboxes: Sandboxes, slowCatchUpMs: number) {
	const names = (repo: string) => {
		const key = createHash("sha256").update(repo).digest("hex").slice(0, 10);
		return { prefix: sandboxes.named(`${key}-`), builder: sandboxes.named(`g-${key}`) };
	};
	// Runs one phase; a failure tries the phase again after a pause, and after the last try gives up to `giveUp`.
	async function retrying(task: RunningTask<BuildInput, BuildState, null>, runtime: TaskRuntime<BuildInput, BuildState, null, object>, context: Context, giveUp: BuildState, body: () => Promise<void>): Promise<void> {
		try {
			await body();
		} catch (e) {
			if (runtime.signal.aborted) throw e;
			const checkpoint = task.state.checkpoint;
			const tries = (checkpoint.tries ?? 0) + 1;
			console.error(`snapshot of ${task.input.repo}, ${checkpoint.phase}: ${(e as Error).message}`);
			if (tries < TRIES) return await runtime.sleepUntil(runtime.now() + RETRY_ROOM_MS, { ...checkpoint, tries }, context);
			await runtime.commit(() => ({ status: "running", checkpoint: giveUp }), context);
		}
	}

	const task = defineTask<BuildInput, BuildState, null>({
		name: "dorf.repo-snapshot",
		version: 1,
		initial: () => ({ phase: "build" }),
		phases: {
			// Reaps first, never last: a Thread that just started may have read the snapshot the doc held until now.
			build: (task, runtime, context) =>
				retrying(task, runtime, context, { phase: "clean" }, async () => {
					const { repo, workspace } = task.input;
					const { prefix, builder } = names(repo);
					const snapshot = (await runtime.snapshot(RepoSnapshots, context))?.repos[repo]?.snapshot;
					await reap(sandboxes, prefix, snapshot);
					await sandboxes.delete(builder);
					if (!(await startFrom(sandboxes, builder, snapshot, false)).woke)
						return await runtime.sleepUntil(runtime.now() + RETRY_ROOM_MS, { phase: "build", tries: task.state.checkpoint.tries }, context);
					const into = `${workspace}/${repo.split("/")[1]}`;
					let sha: string | undefined;
					let failure: string | undefined;
					try {
						sha = await checkout(sandboxes.env(builder, workspace, false), repo, into, context);
						failure = await setup(sandboxes.env(builder, into, false), repo, context);
					} catch (e) {
						if (!(e instanceof CheckoutFailed)) throw e;
						failure = e.message;
					}
					if (failure) {
						console.error(`snapshot of ${repo}: nothing saved, since ${failure}`);
						return await runtime.commit(() => ({ status: "running", checkpoint: { phase: "clean" } }), context);
					}
					await sandboxes.pause(builder, "save");
					// Stamped, so it never takes the name of a snapshot of the same commit that Substrate now refuses.
					const name = `${prefix}${sha!.slice(0, 8)}-${Date.now().toString(36)}`;
					await runtime.commit(() => ({ status: "running", checkpoint: { phase: "save", name, sha: sha! } }), context);
				}),
			// CreateTag is not idempotent: a try cut off leaves a snapshot that is not ready, which goes first.
			save: (task, runtime, context) =>
				retrying(task, runtime, context, { phase: "clean" }, async () => {
					const { name, sha } = task.state.checkpoint as Extract<BuildState, { phase: "save" }>;
					const same = (await sandboxes.snapshots()).find((s) => s.name === name);
					if (same && !same.ready) await sandboxes.deleteSnapshot(name);
					if (!same?.ready) await sandboxes.snapshot(name, names(task.input.repo).builder);
					await runtime.commit(async (tx) => {
						Object.assign(((await tx.doc(RepoSnapshots)).repos[task.input.repo] ??= {}), { snapshot: name, sha });
						return { status: "running", checkpoint: { phase: "clean" } };
					}, context);
				}),
			// `building` clears only once the builder is gone, so the next build cannot race this one's delete.
			clean: (task, runtime, context) => {
				const done = () => runtime.commit(async (tx) => {
					const entry = (await tx.doc(RepoSnapshots)).repos[task.input.repo];
					if (entry?.building === task.id) delete entry.building;
					return { status: "terminal", outcome: { status: "completed", result: null } };
				}, context);
				// A builder the gate would not delete stays for the next build to delete first.
				if ((task.state.checkpoint.tries ?? 0) >= TRIES) return done().then(() => {});
				return retrying(task, runtime, context, { phase: "clean", tries: TRIES }, async () => {
					await sandboxes.delete(names(task.input.repo).builder);
					await done();
				});
			},
		},
		abort: (task, runtime, context) =>
			runtime.commit(async (tx) => {
				const entry = (await tx.doc(RepoSnapshots)).repos[task.input.repo];
				if (entry?.building === task.id) delete entry.building;
				return { status: "terminal", outcome: { status: "aborted" } };
			}, context),
	});

	// In the commit that submits a Thread's first message: a Thread that did not start from the snapshot, or that
	// took longer than `slowCatchUpMs` to catch up from an older commit, starts a build unless one runs. The time stands
	// in for knowing which files a repository's setup reads.
	async function refresh(tx: Tx, input: BuildInput, caughtUp: CatchUp): Promise<void> {
		const entry = ((await tx.doc(RepoSnapshots)).repos[input.repo] ??= {});
		if (entry.building && (await tx.task(entry.building))?.state.status !== "terminal") return;
		if (caughtUp.fromSnapshot && (entry.sha === caughtUp.tip || caughtUp.ms <= slowCatchUpMs)) return;
		entry.building = await tx.createTask(task, input, { ownership: { kind: "conversation" }, conversationId: ROOT_CONVERSATION_ID, background: true });
	}

	return { task, refresh, names, extension: defineExtension({ name: "dorf.repo-snapshot", tasks: [task] }) };
}
