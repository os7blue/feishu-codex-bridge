// Process lifecycle is separate from the fixed-directory CLI so failure paths
// can be exercised without connecting to Feishu or loading private state.
import { spawn } from 'node:child_process';

// Refusal reasons are fixed public codes. A preflight failure must identify an
// active old bridge without exposing raw process arguments or CLI errors.
export function requireBridgeProcessCount(count, offline) {
  if (!Number.isSafeInteger(count) || count < 0) {
    throw Object.assign(new Error('bridge process count unavailable'), { code: 'bridge_process_count_unavailable' });
  }
  if (count !== (offline ? 0 : 1)) {
    throw Object.assign(new Error(offline ? 'offline update requires an already stopped bridge' : 'expected one independent bridge'), {
      code: offline ? 'offline_bridge_still_running' : 'bridge_process_count_unexpected',
    });
  }
}

// The helper loads configuration internally and holds only the existing OS
// state lock. Parent death closes stdin and releases the lock automatically.
export async function holdOfflineLock(executable, args, options = {}) {
  const child = spawn(executable, args, { ...options, stdio: ['pipe','pipe','ignore'] });
  let exited = false, held = false, text = '';
  const done = new Promise(resolve => {
    child.once('error', () => { exited = true; resolve(-1); });
    child.once('exit', code => { exited = true; resolve(code ?? -1); });
  });
  // A closed helper must never raise an unhandled EPIPE in the updater.
  child.stdin.on('error', () => {});
  try {
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('state lock handshake timed out')), 10000);
      const fail = () => { clearTimeout(timer); reject(new Error('state lock unavailable')); };
      child.once('error', fail);
      child.once('exit', fail);
      child.stdout.on('data', chunk => {
        text += chunk;
        if (text.length > 256) return fail();
        if (!text.includes('\n')) return;
        try {
          const report = JSON.parse(text);
          if (report.protocol !== 1 || report.phase !== 'locked' || !Number.isSafeInteger(report.pid) || report.pid < 2) return fail();
          held = true;
          clearTimeout(timer);
          resolve();
        } catch { fail(); }
      });
    });
  } catch {
    child.stdin.destroy();
    // This is only the isolated lock helper, never a bridge process.
    child.kill('SIGTERM');
    await done;
    throw new Error('exclusive maintenance state lock not acquired');
  }
  return {
    closed: done,
    assertHeld() { if (!held || exited) throw new Error('exclusive maintenance state lock lost'); },
    async release() {
      if (!held) return;
      held = false;
      if (exited) throw new Error('maintenance state lock was lost before release');
      child.stdin.end();
      if (await done !== 0) throw new Error('maintenance state lock release not confirmed');
    },
  };
}

export function restartTime(delaySeconds, now = Date.now()) {
  return new Date(now + delaySeconds * 1000).toISOString();
}

// Never persist arbitrary CLI output. Reports are process-bound and contain
// only a protocol version, phase and one of the fixed activity reasons.
export function activityReport(value, pid) {
  const reasons = new Set(['operation_in_progress','active_turn','pending_request','queued_task','file_transfer',
    'incoming_work','outgoing_work','unconfirmed_input','durable_backlog','bridge_failed',
    'receipt_check_failed','backlog_check_failed','shutdown_committed']);
  if (value?.protocol !== 1 || value.pid !== pid || !['idle','busy','unknown','stopping'].includes(value.phase)
      || (value.reason !== undefined && !reasons.has(value.reason))) {
    return { phase: 'unknown', reason: 'control_unavailable' };
  }
  return { phase: value.phase, ...(value.reason ? { reason: value.reason } : {}) };
}

function deferMaintenance(phase) {
  const error = new Error('maintenance deferred');
  error.deferred = true;
  error.reason = phase === 'busy' ? 'bridge_busy' : 'activity_not_confirmed';
  return error;
}

export const reviewedLegacyDigest = 'cc68556d6d397c954b37075f32e5d9085c66d6fbd585c2cc13718b426782d6da';

// This is a distinct, explicitly approved first-rollout path, never a fallback
// from an unavailable activity check. It does not claim the old bridge is idle.
export function validateLegacyBootstrap(proof) {
  if (proof?.interruptionApproved !== true || proof?.binaryDigest !== reviewedLegacyDigest ||
      !Number.isSafeInteger(proof?.pid) || proof.pid < 2 || !/^\d+$/.test(proof?.processStart ?? '')) {
    throw Object.assign(new Error('reviewed legacy interruption approval required'), { code:'legacy_bootstrap_not_approved' });
  }
}

export async function performLegacyBootstrap(ops, proof) {
  validateLegacyBootstrap(proof);
  return maintenanceLifecycle(ops, async () => {
    await ops.save('legacy_stopping', { activity_verified:false, legacy_interruption_explicitly_approved:true });
    const stop = await ops.stopLegacy(proof);
    if (stop?.phase !== 'stopped') throw new Error('legacy shutdown not confirmed');
  });
}

export async function performMaintenance(ops) {
  return maintenanceLifecycle(ops, async () => {
    await ops.save('checking_activity');
    let activity;
    try { activity = await ops.checkActivity(); } catch { throw deferMaintenance('unknown'); }
    if (activity?.phase !== 'idle') throw deferMaintenance(activity?.phase);
    await ops.save('stopping');
    const stop = await ops.stopCurrent();
    if (stop?.phase === 'busy') throw deferMaintenance('busy');
    if (stop?.phase !== 'stopped') throw new Error('idle shutdown not confirmed');
  });
}

async function maintenanceLifecycle(ops, stopCurrent) {
  let stopped = false, attemptedUpdate = false, startAttempted = false, replacement;
  let result;
  try {
    await stopCurrent();
    stopped = true;
    await ops.save('updating');
    attemptedUpdate = true;
    await ops.install();
    await ops.save('starting');
    startAttempted = true;
    replacement = await ops.start(true);
    if (replacement.status !== 'ready') throw new Error('readiness not confirmed');
    const digest = await ops.fingerprint();
    if (digest !== ops.expectedDigest) throw new Error('running binary fingerprint changed');
    await ops.save('ready', { new_pid: replacement.pid, runtime_sha256: digest });
    result = 'ready';
  } catch (error) {
    if (!stopped && error?.deferred === true) {
      await ops.save('deferred', { reason: error.reason, public_files_unchanged: true, bridge_stop_confirmed: false });
      result = 'deferred';
    } else if (!stopped) {
      await ops.save('failed', { reason: 'stop_not_confirmed' });
      result = 'failed';
    } else {
      try {
        // An unresolved launcher may still create a bridge later, even when
        // running() is empty now. It must not race an automatic old-version
        // restart. Only a confirmed exit or identified replacement is safe.
        if (startAttempted && (!replacement || replacement.status === 'unknown')) throw new Error('launcher status unknown');
        const active = await ops.running();
        if (active.length) {
          // Stop only the replacement identified by start(). Never replace
          // public files underneath a live or unidentified bridge process.
          if (!replacement?.pid || active.length !== 1 || active[0] !== replacement.pid) throw new Error('unidentified running process');
          await ops.stopStarted(replacement);
        }
        if ((await ops.running()).length) throw new Error('stop not confirmed');
        if (attemptedUpdate) await ops.rollback();
        // Even a journal failure immediately after stop requires restarting
        // the untouched old executable, without rewriting its public files.
        const old = await ops.start(false);
        await ops.save('rolled_back', {
          rollback_process_running: Boolean(old.pid),
          rollback_connection_ready: false,
        });
        result = 'rolled_back';
      } catch {
        await ops.save('rollback_failed', { reason: 'recovery_not_confirmed' });
        result = 'rollback_failed';
      }
    }
  }
  // Notification failure cannot turn a healthy deployment into a rollback.
  try { await ops.notify(result); } catch { /* The journal remains authoritative. */ }
  return result;
}
