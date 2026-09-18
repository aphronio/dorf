import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { appendFile, readFile, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { Sandbox } from 'e2b';

const execute = promisify(execFile);
const id = process.argv[2];
if (!/^upgrade-proof-[0-9a-f]{12}$/.test(id ?? '')) throw new Error('an exact proof run ID is required');
const root = resolve(import.meta.dir, '../..');
const evidence = resolve(root, '.dorf/runtime-upgrade', id);
const custody = JSON.parse(await readFile(resolve(evidence, 'custody.json'), 'utf8'));
if (custody.key !== id || custody.owner.session_id !== id || custody.owner.sandbox_id !== id || custody.project !== 'dorf-runtime-upgrade-proof') {
  throw new Error('foreign proof custody');
}
async function command(binary: string, args: string[], input = '') {
  const pending = execute(binary, args, { timeout: 330_000 });
  pending.child.stdin?.end(input);
  return (await pending).stdout;
}
async function provider(operation: string) {
  return JSON.parse(await command(resolve(root, '.dorf/bin/runtime-upgrade-provider'), [], JSON.stringify({ ...custody, operation })));
}
async function record(phase: string, detail: Record<string, unknown> = {}) {
  const event = { upgrade_id: id, provider: custody.provider,
    provider_sandbox_id: custody.checkpoint?.source_id, phase, at: new Date().toISOString(), ...detail };
  await appendFile(resolve(evidence, 'events.jsonl'), JSON.stringify(event) + '\n');
}
async function cleanup() {
  if (!custody.checkpoint && custody.capture_started) {
    custody.checkpoint = await provider('capture');
    await writeFile(resolve(evidence, 'custody.json'), JSON.stringify(custody), { mode: 0o600 });
  }
  if (custody.provider === 'e2b') {
    const owned = Sandbox.list({ query: { metadata: { 'dorf.job': id, 'dorf.sandbox': id } } });
    while (owned.hasNext) {
      for (const item of await owned.nextItems()) {
        if (![custody.owner.ownership_nonce, custody.destination.ownership_nonce].includes(item.metadata['dorf.ownership_nonce'])) {
          throw new Error('foreign E2B ownership');
        }
        await Sandbox.kill(item.sandboxId);
      }
    }
    if (custody.checkpoint) await provider('delete');
  } else if (custody.provider === 'incus') {
    const prefix = ['--force-local', '--project', custody.project];
    const projectOwner = await command('incus', [...prefix, 'project', 'get', custody.project, 'user.dorf.proof.owner']);
    if (projectOwner.trim() !== 'dorf-runtime-upgrade-proof-v1') throw new Error('foreign Incus project');
    if (custody.checkpoint) await provider('delete');
    const instances = JSON.parse(await command('incus', [...prefix, 'list', id, '--format=json']));
    for (const instance of instances) {
      if (instance.name !== id || instance.config['user.dorf.ownership_nonce'] !== custody.owner.ownership_nonce) throw new Error('foreign Incus ownership');
      await command('incus', [...prefix, 'delete', id, '--force']);
    }
  } else {
    throw new Error('unknown proof provider');
  }
}
await record('cleanup-recovery.started');
try {
  await cleanup();
  await record('cleanup-recovery.completed');
} catch (error) {
  const raw = error instanceof Error ? error.message : String(error);
  const detail = [process.env.E2B_API_KEY, custody.owner.ownership_nonce, custody.destination.ownership_nonce]
    .filter(Boolean).reduce((value, secret) => value.replaceAll(secret, '[redacted]'), raw);
  await record('cleanup-recovery.failed', { error: detail.slice(-2000) });
  throw new Error(`cleanup recovery failed; see ${evidence}`);
}
await writeFile(resolve(evidence, 'cleanup-recovery.json'), JSON.stringify({ upgrade_id: id, cleanup: 'completed', at: new Date().toISOString() }) + '\n');
console.log(JSON.stringify({ upgrade_id: id, cleanup: 'completed' }));
