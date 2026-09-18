// Bun loads the local development .env into this child without printing keys.
const [provider, operation, session] = process.argv.slice(2);
if (!['incus', 'e2b'].includes(provider)) throw new Error('provider must be incus or e2b');
if (operation && (!['probe', 'cleanup-unstarted'].includes(operation) || !/^job-[a-f0-9]{20}$/.test(session ?? ''))) {
  throw new Error('optional operation: probe SESSION | cleanup-unstarted SESSION');
}
const child = Bun.spawn(['mise', 'exec', '--', 'go', 'test', './internal/postgres',
  '-run', operation ? '^TestLiveUpgradeQuiesceProbe$' : '^TestLiveUpgradeRetainedWorker$',
  '-count=1', '-timeout=30m', '-v'], {
  env: { ...process.env, DORF_LIVE_UPGRADE_PROVIDER: provider, GOFLAGS: '-p=1',
    DORF_UPGRADE_PROBE_SESSION: session ?? '', DORF_UPGRADE_PROBE_ACTION: operation === 'cleanup-unstarted' ? 'cleanup' : '' },
  stdin: 'ignore', stdout: 'inherit', stderr: 'inherit',
});
process.exit(await child.exited);
