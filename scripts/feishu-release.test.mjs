import test from 'node:test';
import assert from 'node:assert/strict';
import { chmod, copyFile, lstat, mkdir, mkdtemp, readFile, symlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { spawn } from 'node:child_process';
import { installPublicRelease, verifyRelease, writeManifest } from './feishu-release.mjs';

async function fixture() {
  const base = await mkdtemp(join(tmpdir(), 'feishu-public-release-test-'));
  const source = join(base, 'source');
  await mkdir(join(source, 'scripts'), { recursive: true });
  const names = ['feishu-bridge', 'scripts/project.mjs', 'config.example.json', 'go.mod', 'go.sum', 'README.md', 'LAUNCH.md'];
  for (const name of names) await writeFile(join(source, name), `public fixture ${name}\n`);
  await chmod(join(source, 'feishu-bridge'), 0o700);
  await writeManifest(source, names);
  return { base, source, target: join(base, 'target') };
}

test('exclusive install copies only manifest-listed public files and leaves private fixtures untouched', async () => {
  const { source, target } = await fixture();
  const privateSentinel = 'fixture only, not a real credential';
  await writeFile(join(source, 'config.json'), privateSentinel);
  await mkdir(join(source, 'state'));
  await writeFile(join(source, 'state/state.json'), privateSentinel);
  const digest = await installPublicRelease(source, target);
  assert.equal(digest.length, 64);
  assert.equal((await verifyRelease(target)).size, 7);
  assert.equal((await lstat(join(target, 'feishu-bridge'))).mode & 0o777, 0o700);
  await assert.rejects(lstat(join(target, 'config.json')), { code: 'ENOENT' });
  await assert.rejects(lstat(join(target, 'state')), { code: 'ENOENT' });
  assert.equal(await readFile(join(source, 'config.json'), 'utf8'), privateSentinel);
  await assert.rejects(installPublicRelease(source, target), { code: 'EEXIST' });
});

test('tampered release and symlink file are refused before target creation', async () => {
  const first = await fixture();
  await writeFile(join(first.source, 'README.md'), 'changed');
  await assert.rejects(installPublicRelease(first.source, first.target), /checksum mismatch/);
  await assert.rejects(lstat(first.target), { code: 'ENOENT' });
  const second = await fixture();
  await symlink(second.source, join(second.base, 'source-link'));
  await assert.rejects(verifyRelease(join(second.base, 'source-link')), /real directory/);
});

test('manifest traversal, private paths, and existing target symlinks are rejected', async () => {
  const { base, source, target } = await fixture();
  await symlink(source, target);
  await assert.rejects(installPublicRelease(source, target), { code: 'EEXIST' });
  for (const name of ['../outside.go', 'config.json', 'state/state.json', 'internal/../secret.go']) {
    await writeFile(join(source, 'MANIFEST.sha256'), `${'a'.repeat(64)}  ${name}\n`);
    await assert.rejects(installPublicRelease(source, join(base, 'unused')), /invalid public release manifest/);
  }
});

test('standalone runner starts with inherited Guard paths and puts caches in its own directory', async () => {
  const { base, source } = await fixture();
  await copyFile(new URL('./project.mjs', import.meta.url), join(source, 'scripts/project.mjs'));
  const fixtureEnv = { ...process.env, GUARD_CLIENT_DATA: join(base, 'guard-original/client'), GUARD_DATA_DIR: join(base, 'guard-original/server') };
  delete fixtureEnv.NODE_TEST_CONTEXT;
  const resultPath = join(base, 'runner-result.json');
  const child = spawn(process.execPath, [join(source, 'scripts/project.mjs'), process.execPath, '-e',
    'require("node:fs").writeFileSync(process.argv[1],JSON.stringify({cwd:process.cwd(),tmp:process.env.TMPDIR,go:process.env.GOMODCACHE,guard:process.env.GUARD_CLIENT_DATA??null}))', resultPath], {
    env: fixtureEnv, stdio: 'ignore',
  });
  const status = await new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', resolve); });
  assert.equal(status, 0);
  assert.deepEqual(JSON.parse(await readFile(resultPath, 'utf8')), { cwd: source, tmp: join(source, '.cache/tmp'), go: join(source, '.cache/go/mod'), guard: null });
});
