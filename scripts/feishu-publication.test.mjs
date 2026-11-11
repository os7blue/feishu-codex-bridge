import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, mkdtemp, writeFile, symlink, unlink } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { tmpdir } from 'node:os';
import { collectPublicSource, publicRootFiles, publicSourceFile } from './feishu-publication.mjs';
import { publicFile } from './feishu-release.mjs';

test('publication excludes internal records and private data while deployment accepts historical documents', () => {
  for (const name of ['AGENTS.md', 'docs/HANDOFF.md', 'docs/PLAN.md', 'docs/REMAINING.md',
    'docs/VALIDATION.md', 'docs/PRODUCT_REVIEW.md', 'docs/PRODUCT_CHANGES.md',
    'docs/history/product-cleanup-baseline-2026-11-11.md', '.cache/token', 'config.json',
    '.env', 'state/state.json', 'SOURCE-MANIFEST.json', 'feishu-bridge', 'internal/../secret.go']) {
    assert.equal(publicSourceFile(name), false, name);
  }
  for (const name of ['README.en.md', 'CONTRIBUTING.md', 'internal/state/state.go', 'cmd/feishu-bridge/main.go']) {
    assert.equal(publicSourceFile(name), true, name);
  }
  assert.equal(publicFile('docs/HANDOFF.md'), true);
  assert.equal(publicFile('scripts/feishu-publication.mjs'), true);
});

async function fixture() {
  const root = await mkdtemp(join(tmpdir(), 'feishu-publication-test-'));
  for (const name of [...publicRootFiles, 'cmd/feishu-bridge/main.go', 'internal/state/state.go',
    'AGENTS.md', 'docs/HANDOFF.md', 'docs/history/private.md', 'config.json', '.cache/token']) {
    await mkdir(dirname(join(root, name)), { recursive: true });
    await writeFile(join(root, name), 'synthetic fixture only\n');
  }
  return root;
}

test('source export retains both source directories but never selects incidental local records', async () => {
  const files = await collectPublicSource(await fixture());
  assert.equal(files.length, publicRootFiles.length + 2);
  assert.ok(files.includes('internal/state/state.go'));
  assert.ok(files.includes('cmd/feishu-bridge/main.go'));
  assert.ok(!files.some(name => name === 'AGENTS.md' || name.includes('HANDOFF') || name.includes('.cache')));
});

test('source export refuses symlinks before reading their target', async () => {
  const root = await fixture();
  await unlink(join(root, 'CONTRIBUTING.md'));
  await symlink(join(root, '.cache/token'), join(root, 'CONTRIBUTING.md'));
  await assert.rejects(collectPublicSource(root), /must be regular/);
});
