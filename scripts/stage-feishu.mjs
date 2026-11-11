import { copyFile, chmod, lstat, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { dirname, join, resolve, relative, isAbsolute, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn } from 'node:child_process';
import { sha256, writeManifest, verifyRelease, verifyArchive } from './feishu-release.mjs';

import { collectPublicSource } from './feishu-publication.mjs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const source = root;
const artifacts = join(root, '.cache/artifacts');
const args = process.argv.slice(2);
if (args.length !== 0 && (args.length !== 2 || args[0] !== '--binary')) throw new Error('usage: stage-feishu.mjs [--binary PROJECT_ARTIFACT]');
const binary = resolve(root, args[1] ?? '.cache/artifacts/feishu-bridge');
const binaryRelative = relative(artifacts, binary);
if (!binaryRelative || binaryRelative === '..' || binaryRelative.startsWith('..' + sep) || isAbsolute(binaryRelative)) throw new Error('binary must remain in project artifacts');
// Resolve all ancestors, not only the final entry, to reject symlink escapes.
let component = artifacts;
for (const name of binaryRelative.split(sep)) {
  if ((await lstat(component)).isSymbolicLink()) throw new Error('artifact symlinks are not allowed');
  component = join(component, name);
}
if (!(await lstat(binary)).isFile() || (await lstat(binary)).isSymbolicLink()) throw new Error('binary must be a regular artifact');
await mkdir(artifacts, { recursive: true, mode: 0o700 });
const directory = await mkdtemp(join(artifacts, 'feishu-release-'));
const files = (await collectPublicSource(source)).filter(name => name !== '.gitignore');
for (const name of files) {
  const destination = join(directory, name);
  await mkdir(dirname(destination), { recursive: true, mode: 0o700 });
  await copyFile(join(source, name), destination);
  await chmod(destination, 0o600);
}
await copyFile(binary, join(directory, 'feishu-bridge'));
await chmod(join(directory, 'feishu-bridge'), 0o700);
files.push('feishu-bridge');
await writeManifest(directory, files);
await verifyRelease(directory);
const archive = `${directory}-linux-amd64.tar.gz`;
const child = spawn('tar', ['-czf', archive, '-C', directory, '.'], { cwd: root, stdio: 'ignore' });
const status = await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', resolve); });
if (status !== 0) throw new Error('public release archive failed');
await chmod(archive, 0o600);
await verifyArchive(directory, archive);
const archiveHash = sha256(await readFile(archive));
await writeFile(`${archive}.sha256`, `${archiveHash}  ${archive.slice(archive.lastIndexOf('/') + 1)}\n`, { mode: 0o600 });
console.log(JSON.stringify({ directory, archive, files: files.length, binary_sha256: sha256(await readFile(join(directory, 'feishu-bridge'))), archive_sha256: archiveHash }));
