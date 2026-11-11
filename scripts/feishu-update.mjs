// Only manifest-listed public files are read or replaced. Unknown files and all
// configuration, credentials, runtime state and logs remain untouched.
import { constants } from 'node:fs';
import { copyFile, chmod, lstat, mkdir, readFile, rename, unlink } from 'node:fs/promises';
import { dirname, join, parse, resolve, sep } from 'node:path';
import { randomUUID } from 'node:crypto';
import { installPublicRelease, verifyRelease, sha256 } from './feishu-release.mjs';

const manifestHash = async directory => sha256(await readFile(join(directory, 'MANIFEST.sha256')));

// Older packages included development records. Retire only these exact,
// manifest-owned paths; unknown files and other removals remain forbidden.
const retiredDevelopmentRecords = new Set([
  'AGENTS.md', 'docs/HANDOFF.md', 'docs/PLAN.md',
  'docs/PRODUCT_CHANGES.md', 'docs/PRODUCT_REVIEW.md',
  'docs/REMAINING.md', 'docs/VALIDATION.md',
  'docs/history/product-cleanup-baseline-2026-11-11.md',
]);

function removedPublicFiles(old, next) {
  const removed = [...old.keys()].filter(name => !next.has(name));
  if (removed.some(name => !retiredDevelopmentRecords.has(name))) {
    const error = new Error('unreviewed public manifest removal');
    error.code = 'public_manifest_removal_rejected';
    throw error;
  }
  return removed;
}

async function safeParents(path) {
  const absolute = resolve(dirname(path));
  let parent = parse(absolute).root;
  for (const component of absolute.slice(parent.length).split(sep).filter(Boolean)) {
    parent = join(parent,component);
    try { await mkdir(parent,{mode:0o700}); } catch(error) { if(error.code!=='EEXIST') throw error; }
    const entry=await lstat(parent);
    if(!entry.isDirectory()||entry.isSymbolicLink()) throw new Error('snapshot parent symlink rejected');
  }
}

async function destination(root, name, known, alternate, partial) {
  if (!(await lstat(root)).isDirectory() || (await lstat(root)).isSymbolicLink()) throw new Error('update root is not a real directory');
  let path = resolve(root);
  const parts = name.split('/');
  for (let i = 0; i < parts.length; i++) {
    path = join(path, parts[i]);
    let entry;
    try { entry = await lstat(path); } catch (error) {
      if (error.code !== 'ENOENT') throw error;
      if (!partial && known.has(name)) throw new Error('installed public file disappeared');
      return;
    }
    if (entry.isSymbolicLink()) throw new Error('update destination symlink rejected');
    if (i < parts.length - 1) {
      if (!entry.isDirectory()) throw new Error('update parent is not a directory');
    } else {
      if (!entry.isFile() || (!known.has(name) && !partial)) throw new Error('unknown destination collision');
      // Never read an unknown colliding file, even to compute a checksum.
      if (!known.has(name) && !alternate.has(name)) throw new Error('unknown public file');
      const digest = sha256(await readFile(path));
      if (digest !== known.get(name) && (!partial || digest !== alternate.get(name))) throw new Error('installed public file changed');
    }
  }
}

export async function preparePublicUpdate(source, target, backup, staging) {
  const next = await verifyRelease(source);
  const old = await verifyRelease(target);
  removedPublicFiles(old, next);
  for (const name of new Set([...old.keys(), ...next.keys()])) await destination(target, name, old, next, false);
  const oldHash = await manifestHash(target);
  await safeParents(backup);
  await safeParents(staging);
  await installPublicRelease(target, backup);
  await installPublicRelease(source, staging);
  if (await manifestHash(target) !== oldHash) throw new Error('installation changed during preparation');
  return { old_manifest_sha256: oldHash, new_manifest_sha256: await manifestHash(staging), binary_sha256: next.get('feishu-bridge') };
}

async function replaceFile(source, target, name) {
  const path = join(target, name);
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const temporary = `${path}.bridge-update-${randomUUID()}`;
  try {
    await copyFile(join(source, name), temporary, constants.COPYFILE_EXCL);
    await chmod(temporary, name === 'feishu-bridge' ? 0o700 : 0o600);
    if (sha256(await readFile(temporary)) !== sha256(await readFile(join(source, name)))) throw new Error('staged copy checksum mismatch');
    await rename(temporary, path);
  } finally {
    try { await unlink(temporary); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
}

export async function applyPublicUpdate(staging, backup, target, expected, { rollback = false, afterFile } = {}) {
  const next = await verifyRelease(staging);
  const old = await verifyRelease(backup);
  if (await manifestHash(backup) !== expected.old_manifest_sha256 || await manifestHash(staging) !== expected.new_manifest_sha256) throw new Error('update snapshots changed');
  const removed = removedPublicFiles(old, next);
  const manifestEntry=await lstat(join(target,'MANIFEST.sha256'));
  if(!manifestEntry.isFile()||manifestEntry.isSymbolicLink()) throw new Error('installed manifest symlink rejected');
  if (!rollback) {
    await verifyRelease(target);
    if(await manifestHash(target) !== expected.old_manifest_sha256) throw new Error('installed manifest changed');
  }
  for (const name of new Set([...old.keys(), ...next.keys()])) await destination(target, name, old, next, rollback);
  const from = rollback ? backup : staging;
  const files = rollback ? old : next;
  for (const name of files.keys()) {
    await replaceFile(from, target, name);
    if (afterFile) await afterFile(name);
  }
  if (rollback) {
    for (const name of next.keys()) {
      if (old.has(name)) continue;
      try { await unlink(join(target, name)); } catch (error) { if (error.code !== 'ENOENT') throw error; }
    }
  } else {
    for (const name of removed) {
      // This file was verified against the old immutable public snapshot.
      // The backup restores it if removal or a later installation step fails.
      await unlink(join(target, name));
      if (afterFile) await afterFile(name);
    }
  }
  // Publish the manifest last. Partial writes are recoverable from two verified
  // immutable snapshots, without enumerating or copying any private directory.
  await replaceFile(from, target, 'MANIFEST.sha256');
  await verifyRelease(target);
}
