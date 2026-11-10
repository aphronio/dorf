// DORF_PROOF_REPO: owner/name of a private repository with a `node --test` suite that dorf-secrets/github can push to.
import { randomUUID } from "node:crypto";
import { call, createThread, fail, killAgent, log, runEnded, startAgent } from "./kit.ts";

const repo = process.env.DORF_PROOF_REPO;
if (!repo || !/^[\w.-]+\/[\w.-]+$/.test(repo)) await fail("set DORF_PROOF_REPO to the repository's owner/name");
const branch = `proof-${randomUUID().slice(0, 8)}`;

await startAgent();

const work = [
	"$ pwd && git log --oneline -1",
	`$ printf 'export const product = (a, b) => a * b;\\n' >> sum.mjs && printf '%s\\n' 'import { product } from "./sum.mjs";' 'test("multiplies two numbers", () => assert.equal(product(2, 3), 6));' >> sum.test.mjs && node --test 2>&1 | grep -E ' (pass|fail) [0-9]+$'`,
	`$ git commit -qam "feat: add product" && git push -q origin HEAD:refs/heads/${branch} && git rev-parse HEAD`,
	`$ gh api repos/${repo}/branches/${branch} --jq .commit.sha`,
	`$ gh pr list --repo ${repo} --limit 1 --json number --jq length`,
	`$ git push -q origin --delete ${branch} && echo deleted`,
].join("\n");
const { id } = await createThread(work, repo);
const [cloned, tested, pushed, rest, graphql, deleted] = await runEnded(id, work, 180_000);
const dir = repo!.split("/")[1];
if (cloned?.error || !cloned?.output.startsWith(`/workspace/${dir}\n`) || !/\n[0-9a-f]{7,} /.test(cloned.output)) await fail(`clone: ${JSON.stringify(cloned)}`);
log("1. the Thread started in its private repository, cloned at create");
if (!/pass 2/.test(tested?.output ?? "") || !/fail 0/.test(tested?.output ?? "")) await fail(`test: ${JSON.stringify(tested)}`);
log("2. changed and tested", "2 tests pass");
const head = pushed?.output.trim().split("\n").at(-1);
if (pushed?.error || !/^[0-9a-f]{40}$/.test(head ?? "")) await fail(`push: ${JSON.stringify(pushed)}`);
if (rest?.output.trim() !== head) await fail(`gh api saw ${JSON.stringify(rest?.output)}, pushed ${head}`);
log("3. committed and pushed; gh's REST call sees the branch", `${branch} at ${head?.slice(0, 12)}`);
if (graphql?.error || !/^\d+$/.test(graphql?.output.trim() ?? "")) await fail(`gh GraphQL: ${JSON.stringify(graphql)}`);
log("4. gh's GraphQL call answered");
if (!deleted?.output.includes("deleted")) await fail(`branch cleanup: ${JSON.stringify(deleted)}`);

await call("DELETE", `/threads/${id}`);
killAgent();
console.log("PASS");
