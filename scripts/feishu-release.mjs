// Public deployment files only. Never traverse a running bridge's data directory.
import { createHash } from 'node:crypto';
import { constants } from 'node:fs';
import { chmod, copyFile, lstat, mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { gunzipSync } from 'node:zlib';

// Compatibility validator for installed historical packages; not a source export policy.
export const publicFile = name => /^(?:feishu-bridge|go\.mod|go\.sum|README(?:\.en)?\.md|CONTRIBUTING\.md|VALIDATION\.md|AGENTS\.md|LAUNCH\.md|config\.example\.json|scripts\/(?:project|stage-feishu|update-feishu|feishu-release|feishu-update|feishu-maintenance|feishu-publication)(?:\.test)?\.mjs|docs\/(?:PLAN|REMAINING|HANDOFF|VALIDATION|AUTHORIZATION|ARCHITECTURE|PRODUCT_REVIEW|PRODUCT_CHANGES|history\/product-cleanup-baseline-2026-11-11)\.md|deploy\/feishu-codex-bridge\.service\.example|(?:cmd|internal)\/[a-zA-Z0-9_/-]+\.go)$/.test(name) && !name.split('/').includes('..');
export const sha256 = data => createHash('sha256').update(data).digest('hex');

export async function sourceFiles(root, directory) {
  const files = [];
  for (const entry of await readdir(join(root, directory), { withFileTypes: true })) {
    const name = `${directory}/${entry.name}`;
    if (entry.isDirectory()) files.push(...await sourceFiles(root, name));
    else if (entry.isFile() && publicFile(name)) files.push(name);
    else throw new Error('unexpected non-public source entry');
  }
  return files.sort();
}

export async function writeManifest(directory, files) {
  const lines = [];
  for (const name of [...files].sort()) {
    if (!publicFile(name)) throw new Error('release contains a non-public file');
    const path = join(directory, name);
    if (!(await lstat(path)).isFile()) throw new Error('release file is not regular');
    lines.push(`${sha256(await readFile(path))}  ${name}`);
  }
  await writeFile(join(directory, 'MANIFEST.sha256'), lines.join('\n') + '\n', { mode: 0o600, flag: 'wx' });
}

export async function verifyRelease(directory) {
  if (!(await lstat(directory)).isDirectory()) throw new Error('release root must be a real directory');
  const manifestPath = join(directory, 'MANIFEST.sha256');
  if (!(await lstat(manifestPath)).isFile()) throw new Error('release manifest must be a regular file');
  const manifest = await readFile(manifestPath, 'utf8');
  const files = new Map();
  for (const line of manifest.trimEnd().split('\n')) {
    const match = /^([a-f0-9]{64})  (.+)$/.exec(line);
    if (!match || !publicFile(match[2]) || files.has(match[2])) throw new Error('invalid public release manifest');
    const [, hash, name] = match;
    let path = resolve(directory);
    for (const component of name.split('/')) {
      path = join(path, component);
      if ((await lstat(path)).isSymbolicLink()) throw new Error('release symlinks are not allowed');
    }
    if (!(await lstat(path)).isFile() || sha256(await readFile(path)) !== hash) throw new Error('release checksum mismatch');
    files.set(name, hash);
  }
  for (const required of ['feishu-bridge', 'scripts/project.mjs', 'config.example.json', 'go.mod', 'go.sum', 'README.md', 'LAUNCH.md']) {
    if (!files.has(required)) throw new Error('incomplete public release');
  }
  return files;
}

export async function installPublicRelease(source, target) {
  const files = await verifyRelease(source);
  // Exclusive mkdir refuses existing directories and symlinks. Existing installs
  // and private files are never overwritten, merged, copied, opened or relocated.
  await mkdir(dirname(target), { recursive: true, mode: 0o700 });
  await mkdir(target, { mode: 0o700 });
  for (const [name, expected] of files) {
    const destination = join(target, name);
    await mkdir(dirname(destination), { recursive: true, mode: 0o700 });
    await copyFile(join(source, name), destination, constants.COPYFILE_EXCL);
    await chmod(destination, name === 'feishu-bridge' ? 0o700 : 0o600);
    if (sha256(await readFile(destination)) !== expected) throw new Error('installed checksum mismatch');
  }
  await copyFile(join(source, 'MANIFEST.sha256'), join(target, 'MANIFEST.sha256'), constants.COPYFILE_EXCL);
  await chmod(join(target, 'MANIFEST.sha256'), 0o600);
  await verifyRelease(target);
  return files.get('feishu-bridge');
}

// Verify the actual gzip/tar bytes without spawning a listing utility. Check
// both headers and file contents, so the archive cannot hide unlisted entries.
export async function verifyArchive(directory, archive) {
  const files = await verifyRelease(directory);
  files.set('MANIFEST.sha256', sha256(await readFile(join(directory, 'MANIFEST.sha256'))));
  const tar = gunzipSync(await readFile(archive));
  const seen = new Set();
  const directories = new Set(['']);
  for (const name of files.keys()) {
    let parent = dirname(name);
    while (parent !== '.') { directories.add(parent); parent = dirname(parent); }
  }
  for (let offset = 0; offset + 512 <= tar.length;) {
    const header = tar.subarray(offset, offset + 512);
    if (header.every(byte => byte === 0)) {
      if (tar.subarray(offset).some(byte => byte !== 0)) throw new Error('unexpected tar trailer');
      if (seen.size !== files.size) throw new Error('incomplete archive');
      return { files: seen.size, archive_sha256: sha256(await readFile(archive)) };
    }
    const field = (start, end) => header.subarray(start, end).toString('utf8').replace(/\0.*$/s, '').trim();
    const checksum = [...header].reduce((sum, byte, index) => sum + (index >= 148 && index < 156 ? 32 : byte), 0);
    if (checksum !== parseInt(field(148, 156), 8)) throw new Error('invalid tar checksum');
    const prefix = field(345, 500);
    const name = (prefix ? `${prefix}/` : '') + field(0, 100);
    const normalized = name.replace(/^\.\//, '').replace(/\/$/, '');
    const size = parseInt(field(124, 136), 8);
    if (!Number.isSafeInteger(size) || size < 0 || offset + 512 + size > tar.length) throw new Error('invalid tar size');
    const kind = header[156];
    if (kind === 53) {
      if (!directories.has(normalized)) throw new Error('unexpected archive directory');
    } else if (kind === 0 || kind === 48) {
      if (!files.has(normalized) || seen.has(normalized) || sha256(tar.subarray(offset + 512, offset + 512 + size)) !== files.get(normalized)) throw new Error('unexpected or altered archive file');
      seen.add(normalized);
    } else {
      throw new Error('archive links and extensions are not allowed');
    }
    offset += 512 + Math.ceil(size / 512) * 512;
  }
  throw new Error('truncated archive');
}
