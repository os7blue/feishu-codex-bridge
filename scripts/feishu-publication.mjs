// New publication selection is stricter than historical deployment validation.
import { lstat } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { execFileSync } from 'node:child_process';
import { sourceFiles } from './feishu-release.mjs';

export const publicRootFiles = Object.freeze([
  '.gitignore', 'go.mod', 'go.sum', 'README.md', 'README.en.md', 'CONTRIBUTING.md',
  'LAUNCH.md', 'VALIDATION.md', 'config.example.json',
  'deploy/feishu-codex-bridge.service.example',
  'docs/ARCHITECTURE.md', 'docs/AUTHORIZATION.md',
  ...['project', 'stage-feishu', 'update-feishu', 'feishu-release', 'feishu-release.test',
    'feishu-update', 'feishu-update.test', 'feishu-maintenance', 'feishu-maintenance.test',
    'feishu-publication', 'feishu-publication.test'].map(name => 'scripts/' + name + '.mjs'),
]);
const roots = new Set(publicRootFiles);
export const publicSourceFile = name => roots.has(name)
  || (/^(?:cmd|internal)\/[a-zA-Z0-9_/-]+\.go$/.test(name) && !name.split('/').includes('..'));

async function regularPath(root, name, directory = false) {
  let path = root;
  const components = name.split('/');
  for (let index = 0; index < components.length; index++) {
    path = join(path, components[index]);
    const entry = await lstat(path);
    if (entry.isSymbolicLink() || ((index < components.length - 1 || directory)
      ? !entry.isDirectory() : !entry.isFile())) throw new Error('publication path must be regular');
  }
}

export async function collectPublicSource(root) {
  for (const directory of ['cmd', 'internal']) await regularPath(root, directory, true);
  const files = [...publicRootFiles, ...await sourceFiles(root, 'cmd'), ...await sourceFiles(root, 'internal')].sort();
  for (const name of files) {
    if (!publicSourceFile(name)) throw new Error('non-public publication path');
    await regularPath(root, name);
  }
  return files;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const root = resolve(fileURLToPath(new URL('..', import.meta.url)));
  const args = process.argv.slice(2);
  if (args.length === 1 && args[0] === '--check-tracked') {
    const names = execFileSync('git', ['ls-files', '-z'], { cwd: root, encoding: 'utf8' }).split('\0').filter(Boolean);
    const rejected = names.filter(name => !publicSourceFile(name));
    if (rejected.length) throw new Error('non-public tracked paths: ' + rejected.join(', '));
    const expected = await collectPublicSource(root);
    if (expected.some(name => !names.includes(name))) throw new Error('required public source is untracked');
    console.log(JSON.stringify({ public_tracked_files: names.length, rejected: 0 }));
  } else if (args.length === 0) {
    console.log(JSON.stringify({ files: await collectPublicSource(root) }));
  } else throw new Error('usage: feishu-publication.mjs [--check-tracked]');
}
