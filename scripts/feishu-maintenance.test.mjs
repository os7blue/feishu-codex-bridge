import test from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { activityReport, holdOfflineLock, performMaintenance, performLegacyBootstrap, validateLegacyBootstrap, reviewedLegacyDigest, restartTime, requireBridgeProcessCount } from './feishu-maintenance.mjs';
import { applyPublicUpdate, preparePublicUpdate } from './feishu-update.mjs';
import { installPublicRelease, sha256, writeManifest } from './feishu-release.mjs';

test('process count preflight reports fixed refusal codes without stopping a bridge', () => {
  assert.doesNotThrow(() => requireBridgeProcessCount(0, true));
  assert.doesNotThrow(() => requireBridgeProcessCount(1, false));
  for (const count of [1,2]) assert.throws(() => requireBridgeProcessCount(count, true), { code:'offline_bridge_still_running' });
  for (const count of [0,2]) assert.throws(() => requireBridgeProcessCount(count, false), { code:'bridge_process_count_unexpected' });
  for (const count of [-1,NaN,1.5]) assert.throws(() => requireBridgeProcessCount(count, true), { code:'bridge_process_count_unavailable' });
});

function fixture(options = {}) {
  const trace = [], records = [];
  let active = [11];
  const ops = {
    expectedDigest: 'new',
    checkActivity: async () => {
      trace.push('check-live');
      if (options.controlFails) throw new Error('fixture private-error');
      return { phase: options.busy ? 'busy' : options.unknown ? 'unknown' : 'idle' };
    },
    save: async (phase, extra) => {
      trace.push(phase);
      if (phase === options.failSave) throw new Error('fixture journal failure');
      records.push({ phase, ...extra });
    },
    stopCurrent: async () => {
      trace.push('stop-old');
      if (options.becameBusy) return { phase: 'busy' };
      if (options.stopUnknown) return { phase: 'unknown' };
      if (options.stopFails) throw new Error('fixture stop timeout');
      active = [];
      return { phase: 'stopped' };
    },
    install: async () => {
      assert.equal(active.length, 0);
      trace.push('install');
      if (options.installFails) throw new Error('fixture partial write');
    },
    start: async next => {
      assert.equal(active.length, 0);
      trace.push(next ? 'start-new' : 'start-old');
      if (next && options.startThrows) { active = [33]; throw new Error('fixture lost identity'); }
      if (next && options.launcherThrows) throw new Error('fixture unresolved launcher');
      if (next && options.unknownLauncher) return { status: 'unknown' };
      if (next && options.exits) return { status: 'exited' };
      if (!next && options.oldExits) return { status: 'exited' };
      const pid = next ? 22 : 44;
      active = [pid];
      return { status: next && !options.noReady ? 'ready' : 'unconfirmed', pid, stamp: 'fixture' };
    },
    running: async () => active,
    stopStarted: async replacement => {
      trace.push('stop-new');
      assert.deepEqual(active, [replacement.pid]);
      if (options.newStopFails) throw new Error('fixture replacement stop timeout');
      active = [];
    },
    fingerprint: async () => options.badHash ? 'unexpected' : 'new',
    rollback: async () => { assert.equal(active.length, 0); trace.push('restore'); },
    notify: async phase => { trace.push(`notify-${phase}`); if (options.notifyFails) throw new Error('fixture notification failure'); },
  };
  return { ops, trace, records };
}

const approvedLegacy = {interruptionApproved:true,binaryDigest:reviewedLegacyDigest,pid:11,processStart:'123'};

test('legacy bootstrap requires explicit interruption approval bound to the reviewed binary and process', async () => {
  for(const proof of [{},{...approvedLegacy,interruptionApproved:false},{...approvedLegacy,binaryDigest:'unknown'},{...approvedLegacy,pid:0},{...approvedLegacy,processStart:''}]) {
    const f=fixture();
    await assert.rejects(performLegacyBootstrap(f.ops,proof),{code:'legacy_bootstrap_not_approved'});
    assert.deepEqual(f.trace,[]);
  }
  assert.doesNotThrow(()=>validateLegacyBootstrap(approvedLegacy));
});

test('approved first rollout records unverified activity and reuses guarded replacement and readiness checks', async () => {
  const f=fixture();
  f.ops.stopLegacy=async proof=>{assert.deepEqual(proof,approvedLegacy);f.trace.push('legacy-approved');return f.ops.stopCurrent();};
  assert.equal(await performLegacyBootstrap(f.ops,approvedLegacy),'ready');
  assert.equal(f.trace.includes('check-live'),false);
  assert.equal(f.records[0].phase,'legacy_stopping');
  assert.equal(f.records[0].activity_verified,false);
  assert.equal(f.records[0].legacy_interruption_explicitly_approved,true);
  assert.equal(f.records.at(-1).runtime_sha256,'new');
});

test('unconfirmed legacy stop never installs or starts another bridge', async () => {
  const f=fixture();f.ops.stopLegacy=async()=>({phase:'unknown'});
  assert.equal(await performLegacyBootstrap(f.ops,approvedLegacy),'failed');
  for(const step of ['install','start-new','start-old','restore'])assert.equal(f.trace.includes(step),false);
});

test('legacy rollout restores public files when replacement fails before readiness', async () => {
  const f=fixture({noReady:true});f.ops.stopLegacy=()=>f.ops.stopCurrent();
  assert.equal(await performLegacyBootstrap(f.ops,approvedLegacy),'rolled_back');
  assert.ok(f.trace.indexOf('stop-new')<f.trace.indexOf('restore'));
  assert.ok(f.trace.indexOf('restore')<f.trace.indexOf('start-old'));
});

test('restart delay is calculated from completed preflight time', () => {
  const afterSlowPreflight = Date.UTC(2026, 10, 9, 8, 20);
  assert.equal(Date.parse(restartTime(240, afterSlowPreflight)) - afterSlowPreflight, 240000);
});

test('successful maintenance confirms readiness and hash before notification', async () => {
  const f = fixture();
  assert.equal(await performMaintenance(f.ops), 'ready');
  assert.deepEqual(f.trace, ['checking_activity','check-live','stopping','stop-old','updating','install','starting','start-new','ready','notify-ready']);
  assert.equal(f.records.at(-1).runtime_sha256, 'new');
});

test('failed old process stop never installs, restarts, or forces a kill', async () => {
  const f = fixture({ stopFails: true });
  assert.equal(await performMaintenance(f.ops), 'failed');
  assert.deepEqual(f.trace, ['checking_activity','check-live','stopping','stop-old','failed','notify-failed']);
});

test('partial replacement failure restores old public files before starting old process', async () => {
  const f = fixture({ installFails: true });
  assert.equal(await performMaintenance(f.ops), 'rolled_back');
  assert.deepEqual(f.trace, ['checking_activity','check-live','stopping','stop-old','updating','install','restore','start-old','rolled_back','notify-rolled_back']);
});

test('journal failure after shutdown restarts untouched old executable', async () => {
  const f = fixture({ failSave: 'updating' });
  assert.equal(await performMaintenance(f.ops), 'rolled_back');
  assert.ok(f.trace.includes('start-old'));
  assert.ok(!f.trace.includes('install'));
  assert.ok(!f.trace.includes('restore'));
});

test('readiness timeout, bad hash and ready journal failure stop replacement before rollback', async () => {
  for (const options of [{ noReady: true }, { badHash: true }, { failSave: 'ready' }]) {
    const f = fixture(options);
    assert.equal(await performMaintenance(f.ops), 'rolled_back');
    assert.ok(f.trace.indexOf('stop-new') < f.trace.indexOf('restore'));
    assert.ok(f.trace.indexOf('restore') < f.trace.indexOf('start-old'));
    assert.equal(f.records.at(-1).rollback_connection_ready, false);
  }
});

test('exited replacement is rolled back and exited old process is not reported running', async () => {
  const f = fixture({ exits: true, oldExits: true });
  assert.equal(await performMaintenance(f.ops), 'rolled_back');
  assert.ok(!f.trace.includes('stop-new'));
  assert.equal(f.records.at(-1).rollback_process_running, false);
});

test('unknown live process or unconfirmed replacement stop prevents file rollback and duplicate start', async () => {
  for (const options of [{ startThrows: true }, { noReady: true, newStopFails: true }]) {
    const f = fixture(options);
    assert.equal(await performMaintenance(f.ops), 'rollback_failed');
    assert.ok(!f.trace.includes('restore'));
    assert.ok(!f.trace.includes('start-old'));
  }
});

test('unresolved launcher cannot race rollback even when no bridge is visible yet', async () => {
  for (const options of [{ unknownLauncher: true }, { launcherThrows: true }]) {
    const f = fixture(options);
    assert.equal(await performMaintenance(f.ops), 'rollback_failed');
    assert.ok(!f.trace.includes('restore'));
    assert.ok(!f.trace.includes('start-old'));
  }
});

test('notification failure keeps the verified deployment ready', async () => {
  const f = fixture({ notifyFails: true });
  assert.equal(await performMaintenance(f.ops), 'ready');
  assert.equal(f.records.at(-1).phase, 'ready');
  assert.ok(!f.trace.includes('restore'));
});

test('busy, unknown and unavailable live control defer without shutdown or replacement', async () => {
  for (const options of [{ busy: true }, { unknown: true }, { controlFails: true }]) {
    const f = fixture(options);
    assert.equal(await performMaintenance(f.ops), 'deferred');
    assert.deepEqual(f.trace, ['checking_activity','check-live','deferred','notify-deferred']);
    assert.equal(f.records.at(-1).public_files_unchanged, true);
    assert.equal(f.records.at(-1).bridge_stop_confirmed, false);
    assert.ok(!JSON.stringify(f.records).includes('private-error'));
  }
});

test('offline helper holds until explicit release and detects unexpected exit', async () => {
  const code = 'import {writeSync} from "node:fs";writeSync(1,JSON.stringify({protocol:1,phase:"locked",pid:process.pid})+"\\n");const keep=setInterval(()=>{},1000);process.stdin.resume();process.stdin.on("end",()=>{clearInterval(keep);process.exit(0)});';
  const dir = await mkdtemp(join(tmpdir(), 'feishu-lock-helper-'));
  const helper = join(dir, 'helper.mjs'), lostHelper = join(dir, 'lost.mjs');
  await writeFile(helper, code);
  await writeFile(lostHelper, code + 'setTimeout(()=>process.exit(1),100);');
  const guard = await holdOfflineLock(process.execPath, [helper]);
  guard.assertHeld();
  await guard.release();
  assert.throws(() => guard.assertHeld());
  const lost = await holdOfflineLock(process.execPath, [lostHelper]);
  await lost.closed;
  assert.throws(() => lost.assertHeld());
  await assert.rejects(lost.release());
});

test('offline helper rejects unavailable, malformed and wrong-protocol handshakes', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'feishu-lock-invalid-'));
  let index = 0;
  for (const code of ['process.exit(1)', 'import {writeSync} from "node:fs";writeSync(1,"private-invalid\\n");', 'import {writeSync} from "node:fs";writeSync(1,JSON.stringify({protocol:2,phase:"locked",pid:process.pid})+"\\n");']) {
    const helper = join(dir, `${index++}.mjs`);
    await writeFile(helper, code);
    await assert.rejects(holdOfflineLock(process.execPath, [helper]), /state lock not acquired/);
  }
});

test('work arriving after the status check is preserved by atomic stop refusal', async () => {
  for (const options of [{ becameBusy: true }]) {
    const f = fixture(options);
    assert.equal(await performMaintenance(f.ops), 'deferred');
    assert.deepEqual(f.trace, ['checking_activity','check-live','stopping','stop-old','deferred','notify-deferred']);
    assert.ok(!f.trace.includes('install'));
    assert.ok(!f.trace.includes('restore'));
    assert.ok(!f.trace.includes('start-old'));
  }
});

test('missing guard and unconfirmed stop never imply a safe shutdown', async () => {
  const f = fixture();
  delete f.ops.checkActivity;
  assert.equal(await performMaintenance(f.ops), 'deferred');
  assert.ok(!f.trace.includes('stop-old'));
  const g = fixture();
  g.ops.stopCurrent = async () => undefined;
  assert.equal(await performMaintenance(g.ops), 'failed');
  assert.ok(!g.trace.includes('install'));
  const h = fixture({ stopUnknown: true });
  assert.equal(await performMaintenance(h.ops), 'failed');
  assert.equal(h.records.at(-1).reason, 'stop_not_confirmed');
  assert.ok(!h.trace.includes('install'));
});

test('activity reports reject wrong processes, unknown protocol and unsafe fields', () => {
  for (const value of [null, { protocol: 2, pid: 12, phase: 'idle' }, { protocol: 1, pid: 13, phase: 'idle' },
    { protocol: 1, pid: 12, phase: 'ready' }, { protocol: 1, pid: 12, phase: 'busy', reason: 'private-payload' }]) {
    assert.deepEqual(activityReport(value, 12), { phase: 'unknown', reason: 'control_unavailable' });
  }
  assert.deepEqual(activityReport({ protocol: 1, pid: 12, phase: 'busy', reason: 'pending_request', secret: 'never copied' }, 12),
    { phase: 'busy', reason: 'pending_request' });
});

test('actual local subprocess shutdown, public file switch, startup and rollback use the lifecycle', async t => {
  for (const noReady of [false, true]) {
    await t.test(noReady ? 'missing receipt restores old version' : 'ready replacement stays installed', async () => {
      const base = await mkdtemp(join(tmpdir(), 'feishu-lifecycle-test-'));
      const old = join(base, 'old'), next = join(base, 'next'), target = join(base, 'target');
      const names = ['feishu-bridge','scripts/project.mjs','config.example.json','go.mod','go.sum','README.md','LAUNCH.md'];
      for (const [directory, version] of [[old,'old'],[next,'new']]) {
        await mkdir(join(directory, 'scripts'), { recursive: true });
        for (const name of names) await writeFile(join(directory, name), `${version} public ${name}`);
        await writeManifest(directory, names);
      }
      await installPublicRelease(old, target);
      // These are synthetic fixtures, never real bridge configuration or state.
      await writeFile(join(target, 'config.json'), 'private fixture config');
      await mkdir(join(target, 'state'));
      await writeFile(join(target, 'state/session.json'), 'private fixture session');
      const backup = join(base, 'backup'), staging = join(base, 'staging');
      const expected = await preparePublicUpdate(next, target, backup, staging);
      const children = new Map();
      const receipt = join(base, 'ready.json');
      async function launch(writeReceipt) {
        const child = spawn(process.execPath, ['-e',
          'if(process.argv[1])require("node:fs").writeFileSync(process.argv[1],JSON.stringify({ready:true,pid:process.pid}));setInterval(()=>{},1000)',
          writeReceipt ? receipt : '',
        ], { stdio: 'ignore' });
        const exit = once(child, 'exit');
        await once(child, 'spawn');
        children.set(child.pid, { child, exit });
        return child.pid;
      }
      async function stop(pid) {
        const { child, exit } = children.get(pid);
        child.kill('SIGTERM');
        await exit;
        children.delete(pid);
      }
      try {
        const oldPid = await launch(false);
        const records = [];
        const result = await performMaintenance({
          expectedDigest: expected.binary_sha256,
          checkActivity: async () => ({ phase: 'idle' }),
          save: async (phase, extra) => { records.push({ phase, ...extra }); },
          stopCurrent: async () => { await stop(oldPid); return { phase: 'stopped' }; },
          install: async () => {
            assert.equal(children.size, 0);
            await applyPublicUpdate(staging, backup, target, expected);
          },
          start: async isNew => {
            assert.equal(children.size, 0);
            const pid = await launch(isNew && !noReady);
            if (isNew && !noReady) {
              // Poll only the fixture receipt, never a real bridge log.
              let ready;
              for (let i = 0; i < 100; i++) {
                try { ready = JSON.parse(await readFile(receipt, 'utf8')); break; } catch {}
                await new Promise(resolve => setTimeout(resolve, 10));
              }
              assert.deepEqual(ready, { ready: true, pid });
              return { status: 'ready', pid };
            }
            return { status: 'unconfirmed', pid };
          },
          running: async () => [...children.keys()],
          stopStarted: child => stop(child.pid),
          fingerprint: async () => sha256(await readFile(join(target, 'feishu-bridge'))),
          rollback: async () => {
            assert.equal(children.size, 0);
            await applyPublicUpdate(staging, backup, target, expected, { rollback: true });
          },
          notify: async () => {},
        });
        assert.equal(result, noReady ? 'rolled_back' : 'ready');
        assert.equal(children.size, 1);
        assert.equal(await readFile(join(target, 'feishu-bridge'), 'utf8'), `${noReady ? 'old' : 'new'} public feishu-bridge`);
        assert.equal(await readFile(join(target, 'config.json'), 'utf8'), 'private fixture config');
        assert.equal(await readFile(join(target, 'state/session.json'), 'utf8'), 'private fixture session');
        assert.equal(records.at(-1).phase, result);
      } finally {
        for (const pid of children.keys()) await stop(pid);
      }
    });
  }
});
