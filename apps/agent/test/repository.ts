// A Thread works on a real repository: clone a private one, change it, test, commit and push, with
// the push credential injected by Substrate's egress gateway. DORF_PROOF_REPO is an HTTPS GitHub
// URL of a repository with a `node --test` suite that the dorf-secrets/github token can push to.
import { randomUUID } from "node:crypto";
import { call, createThread, fail, killAgent, log, runEnded, startAgent } from "./kit.ts";

const repo = process.env.DORF_PROOF_REPO;
if (!repo?.startsWith("https://github.com/")) await fail("set DORF_PROOF_REPO to the repository's https://github.com/... URL");
const branch = `proof-${randomUUID().slice(0, 8)}`;

await startAgent();

const work = [
	`$ git clone -q ${repo} repo && git -C repo log --oneline -1`,
	`$ cd repo && printf 'export const product = (a, b) => a * b;\\n' >> sum.mjs && printf '%s\\n' 'import { product } from "./sum.mjs";' 'test("multiplies two numbers", () => assert.equal(product(2, 3), 6));' >> sum.test.mjs && node --test 2>&1 | grep -E ' (pass|fail) [0-9]+$'`,
	`$ cd repo && git -c user.name=Dorf -c user.email=dorf@users.noreply.github.com commit -qam "feat: add product" && git push -q origin HEAD:refs/heads/${branch} && git rev-parse HEAD`,
	`$ cd repo && git ls-remote origin refs/heads/${branch} | cut -f1`,
	`$ cd repo && git push -q origin --delete ${branch} && echo deleted`,
].join("\n");
const { id } = await createThread(work);
const [cloned, tested, pushed, remote, deleted] = await runEnded(id, work, 180_000);
if (cloned?.error || !/^[0-9a-f]{7,} /.test(cloned?.output ?? "")) await fail(`clone: ${JSON.stringify(cloned)}`);
log("1. private repository cloned");
if (!/pass 2/.test(tested?.output ?? "") || !/fail 0/.test(tested?.output ?? "")) await fail(`test: ${JSON.stringify(tested)}`);
log("2. changed and tested", "2 tests pass");
const head = pushed?.output.trim().split("\n").at(-1);
if (pushed?.error || !/^[0-9a-f]{40}$/.test(head ?? "")) await fail(`push: ${JSON.stringify(pushed)}`);
if (remote?.output.trim() !== head) await fail(`remote branch is ${JSON.stringify(remote?.output)}, pushed ${head}`);
log("3. committed and pushed", `branch ${branch} at ${head?.slice(0, 12)} on the remote`);
if (!deleted?.output.includes("deleted")) await fail(`branch cleanup: ${JSON.stringify(deleted)}`);

await call("DELETE", `/threads/${id}`);
killAgent();
console.log("PASS");
