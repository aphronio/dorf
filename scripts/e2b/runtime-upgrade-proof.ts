import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { randomBytes } from 'node:crypto';
import { appendFile, mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { Sandbox } from 'e2b';

const execute = promisify(execFile);
const exec: typeof execute = (...args: Parameters<typeof execute>) => {
  const pending = execute(...args);
  pending.child.stdin?.end();
  return pending;
};
const provider = process.argv[2];
if (!['incus', 'e2b'].includes(provider)) throw new Error('usage: runtime-upgrade-proof.ts incus|e2b');
const loseCheckpointResponse = process.argv[3] === '--lose-checkpoint-response';
if ((process.argv[3] && !loseCheckpointResponse) || process.argv.length > 4) throw new Error('unknown proof argument');
const id = `upgrade-proof-${randomBytes(6).toString('hex')}`;
const root = resolve(import.meta.dir, '../..');
const evidence = resolve(root, '.dorf/runtime-upgrade', id);
const guest = '/opt/dorf-upgrade-proof';
const project = 'dorf-runtime-upgrade-proof';
const owner = 'dorf-runtime-upgrade-proof-v1';
const sourceOwner = { job_id: id, sandbox_id: id, ownership_nonce: randomBytes(32).toString('hex') };
const destinationOwner = { ...sourceOwner, ownership_nonce: provider === 'incus' ? sourceOwner.ownership_nonce : randomBytes(32).toString('hex') };
const manifestPath = resolve(root, 'dist/release-0.10.1/dorf-incus-vm-v5-x86_64.json');
const templatePath = resolve(root, 'dist/e2b-template/profile.json');
let sandbox: Sandbox | undefined;
let providerID = id;
let snapshot = '';
let savedCheckpoint: { key: string; reference: string; source_id: string } | undefined;
let instanceCreated = false;
let succeeded = false;
let captureStarted = false;
const resources = new Set<string>();
const quote = (value: string) => `'${value.replaceAll("'", "'\\''")}'`;
await mkdir(evidence, { recursive: true });

async function persistCustody() {
  await writeFile(resolve(evidence, 'custody.json'), JSON.stringify({ provider, project, key: id,
    owner: sourceOwner, destination: destinationOwner, checkpoint: savedCheckpoint, capture_started: captureStarted }), { mode: 0o600 });
}
await persistCustody();

async function record(phase: string, detail: Record<string, unknown> = {}) {
  const event = { upgrade_id: id, provider, provider_sandbox_id: providerID, phase, at: new Date().toISOString(), ...detail };
  await appendFile(resolve(evidence, 'events.jsonl'), JSON.stringify(event) + '\n');
  console.log(JSON.stringify(event));
}
async function incus(...args: string[]) {
  return (await exec('incus', ['--force-local', '--project', project, ...args], { timeout: 600_000, maxBuffer: 16 * 1024 * 1024 })).stdout;
}
async function providerOperation(operation: string) {
  const pending = execute(resolve(root, '.dorf/bin/runtime-upgrade-provider'), [], { timeout: 330_000 });
  pending.child.stdin?.end(JSON.stringify({ provider, operation, project, key: id, owner: sourceOwner,
    destination: destinationOwner, checkpoint: savedCheckpoint }));
  return JSON.parse((await pending).stdout);
}
async function run(command: string) {
  if (provider === 'incus') return incus('exec', id, '--', 'bash', '-lc', command);
  const result = await sandbox!.commands.run(command, { user: 'root', timeoutMs: 600_000 });
  if (result.exitCode !== 0) throw new Error(`guest command failed (${result.exitCode}): ${result.stderr.slice(-1200)}`);
  return result.stdout;
}
async function step(name: string, work: () => Promise<unknown>) {
  const started = Date.now();
  await record(name + '.started');
  try {
    const result = await work();
    await writeFile(resolve(evidence, name + '.log'), typeof result === 'string' ? result : JSON.stringify(result ?? null));
    await record(name + '.completed', { duration_ms: Date.now() - started });
    return result;
  } catch (error) {
    const raw = error instanceof Error ? error.message + ('stderr' in error ? '\n' + error.stderr : '') : String(error);
    const detail = [process.env.E2B_API_KEY, sourceOwner.ownership_nonce, destinationOwner.ownership_nonce]
      .filter(Boolean).reduce((value, secret) => value.replaceAll(secret!, '[redacted]'), raw);
    await record(name + '.failed', { duration_ms: Date.now() - started, error: detail.slice(-2000) });
    throw new Error(`${name} failed; see ${evidence}`);
  }
}
async function ready() {
  const end = Date.now() + 120_000;
  while (Date.now() < end) {
    try { await run('true'); return; } catch { await Bun.sleep(1000); }
  }
  throw new Error('guest did not become ready');
}
async function create() {
  if (provider === 'e2b') {
    if (!process.env.E2B_API_KEY) throw new Error('E2B_API_KEY is required');
    const manifest = JSON.parse(await readFile(templatePath, 'utf8'));
    sandbox = await Sandbox.create(manifest.template.reference, {
      timeoutMs: 3_600_000, metadata: { 'dorf.owner': 'sandbox', 'dorf.job': id, 'dorf.sandbox': id,
        'dorf.ownership_nonce': sourceOwner.ownership_nonce, 'dorf.upgrade.proof': id }, allowInternetAccess: true,
    });
    providerID = sandbox.sandboxId;
    resources.add(providerID);
    return;
  }
  const projects = JSON.parse((await exec('incus', ['--force-local', 'project', 'list', '--format=json'])).stdout);
  const existing = projects.find((item: { name: string }) => item.name === project);
  if (existing) {
    if (existing.config['user.dorf.proof.owner'] !== owner) throw new Error('foreign proof project');
  } else {
    await exec('incus', ['--force-local', 'project', 'create', project,
      '-c', `user.dorf.proof.owner=${owner}`, '-c', 'features.images=true', '-c', 'features.profiles=false']);
  }
  const manifest = JSON.parse(await readFile(manifestPath, 'utf8'));
  try { await incus('image', 'info', manifest.image_fingerprint); }
  catch { await incus('image', 'import', resolve(manifestPath, '..', manifest.archive.name)); }
  instanceCreated = true;
  await incus('launch', manifest.image_fingerprint, id, '--vm', '--network', 'incusbr0', '--storage', 'default',
    '-d', 'root,size=50GiB', '-c', 'limits.memory=4GiB', '-c', 'limits.cpu=2', '-c', `user.dorf.proof.run=${id}`,
    '-c', 'user.dorf.owner=sandbox', '-c', `user.dorf.job=${id}`, '-c', `user.dorf.sandbox=${id}`,
    '-c', `user.dorf.ownership_nonce=${sourceOwner.ownership_nonce}`);
  await ready();
}
async function copyRecipes() {
  await run(`mkdir -p ${guest}`);
  for (const name of ['guest.sh', 'package.nix', 'packages.json', 'native-session.py']) {
    const path = resolve(root, name === 'native-session.py' ? 'scripts/runtime-upgrade' : 'scripts/sandbox/packages', name);
    if (provider === 'incus') await incus('file', 'push', path, `${id}${guest}/${name}`);
    else await sandbox!.files.write(`${guest}/${name}`, await readFile(path), { user: 'root' });
  }
}
async function checkpoint() {
  captureStarted = true;
  await persistCustody();
  const captured = await providerOperation('capture');
  if (loseCheckpointResponse) throw new Error('injected loss of accepted checkpoint response');
  savedCheckpoint = captured;
  snapshot = savedCheckpoint!.reference;
  await persistCustody();
  const retried = await providerOperation('capture');
  if (JSON.stringify(retried) !== JSON.stringify(savedCheckpoint)) throw new Error('capture retry changed checkpoint identity');
  await ready();
  return savedCheckpoint;
}
async function restore() {
  const restored = await providerOperation('restore');
  if (provider === 'e2b') {
    const previous = sandbox!;
    sandbox = await Sandbox.connect(restored.provider_id, { timeoutMs: 3_600_000 });
    providerID = sandbox.sandboxId;
    resources.add(providerID);
    const retried = await providerOperation('restore');
    if (retried.provider_id !== providerID) throw new Error('restore retry created another VM');
    await previous.kill();
    resources.delete(previous.sandboxId);
  }
  await ready();
  return { provider_sandbox_id: providerID };
}
const activate = (version: string) => run(`bash ${guest}/guest.sh activate ${quote(version)}`);
const native = (operation: string) => run(`python3 ${guest}/native-session.py ${operation}`);
const digestState = () => run(`python3 - <<'PY'
import hashlib
from pathlib import Path
root=Path('/workspace/upgrade-proof')
d=hashlib.sha256()
for p in sorted(root.rglob('*')):
 if p.is_file():
  d.update(str(p.relative_to(root)).encode()+b'\\0'+p.read_bytes()+b'\\0')
print(d.hexdigest())
PY`);
try {
  await step('create', create);
  await step('copy', copyRecipes);
  await step('storage-coverage', async () => {
    if (provider === 'incus') {
      const instances = JSON.parse(await incus('list', id, '--format=json'));
      const detail = instances.find((instance: { name: string }) => instance.name === id);
      if (!detail || detail.config['user.dorf.proof.run'] !== id) throw new Error('VM ownership changed');
      const devices = Object.values(detail.expanded_devices) as { type: string; path?: string }[];
      if (devices.some(device => device.type === 'disk' && device.path && device.path !== '/')) throw new Error('snapshot excludes an attached volume');
    } else {
      const detail = await sandbox!.getInfo();
      if (detail.volumeMounts?.length) throw new Error('snapshot coverage for mounted E2B volumes is not proved');
    }
    return run('findmnt -T /workspace -n -o SOURCE,FSTYPE,TARGET');
  });
  await step('bootstrap', () => run(`bash ${guest}/guest.sh bootstrap`));
  // Fetch and build both immutable closures once; the maintenance window only activates.
  await step('stage-new', () => run(`bash ${guest}/guest.sh stage 0.154.0`));
  await step('stage-old', () => run(`bash ${guest}/guest.sh stage 0.147.0`));
  await step('activate-old', () => activate('0.147.0'));
  const original = JSON.parse(String(await step('create-session', () => native('create'))));
  const originalDigest = (await digestState()).trim();
  await step('checkpoint', checkpoint);
  await step('upgrade', () => activate('0.154.0'));
  const upgraded = JSON.parse(String(await step('verify-upgrade', () => native('resume'))));
  if (upgraded.thread_id !== original.thread_id) throw new Error('upgrade changed native thread identity');
  await step('simulate-state-migration-failure', () => run("printf 'incompatible upgraded state' > /workspace/upgrade-proof/migrated-state"));
  await record('verification.failed', { reason: 'injected post-migration failure' });
  await step('rollback', restore);
  const restoredDigest = (await digestState()).trim();
  if (restoredDigest !== originalDigest) throw new Error('snapshot did not restore the exact pre-upgrade local state');
  const recovered = JSON.parse(String(await step('verify-rollback', () => native('resume'))));
  if (recovered.thread_id !== original.thread_id || !recovered.version.includes('0.147.0')) throw new Error('rollback did not restore original runner and conversation');
  await record('proof.passed', { original_state_sha256: originalDigest, restored_state_sha256: restoredDigest, thread_id: original.thread_id });
  succeeded = true;
} finally {
  let cleanupSucceeded = false;
  try {
    await step('cleanup', async () => {
      // Retain the source so the recovery recipe can reconcile a lost capture response.
      if (captureStarted && !savedCheckpoint) throw new Error('checkpoint outcome is unknown; run the cleanup recipe for this proof ID');
      if (provider === 'incus' && snapshot) {
        await providerOperation('delete');
        await providerOperation('delete');
      }
      if (provider === 'incus' && instanceCreated) {
        const instances = JSON.parse(await incus('list', id, '--format=json'));
        for (const instance of instances) {
          if (instance.name !== id || instance.config['user.dorf.proof.run'] !== id) throw new Error('refusing cleanup of unowned VM');
          await incus('delete', id, '--force');
        }
      }
      if (provider === 'e2b') {
        const owned = Sandbox.list({ query: { metadata: { 'dorf.job': id, 'dorf.sandbox': id } } });
        while (owned.hasNext) {
          for (const item of await owned.nextItems()) {
            if (![sourceOwner.ownership_nonce, destinationOwner.ownership_nonce].includes(item.metadata['dorf.ownership_nonce'])) throw new Error('refusing cleanup of unowned E2B VM');
            resources.add(item.sandboxId);
          }
        }
      }
      for (const resource of resources) await Sandbox.kill(resource);
      if (provider === 'e2b' && snapshot) {
        await providerOperation('delete');
        await providerOperation('delete');
      }
    });
    cleanupSucceeded = true;
  } finally {
    await record('proof.finished', { succeeded: succeeded && cleanupSucceeded, proof_succeeded: succeeded, cleanup_succeeded: cleanupSucceeded, evidence });
  }
}
