import test from 'node:test';
import assert from 'node:assert/strict';
import { chmod, mkdir, mkdtemp, readFile, symlink, unlink, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { installPublicRelease, verifyRelease, writeManifest } from './feishu-release.mjs';
import { applyPublicUpdate, preparePublicUpdate } from './feishu-update.mjs';

async function fixture() {
  const base = await mkdtemp(join(tmpdir(), 'feishu-update-test-'));
  const old = join(base, 'old'), next = join(base, 'next'), target = join(base, 'target');
  const names = ['feishu-bridge','scripts/project.mjs','config.example.json','go.mod','go.sum','README.md','LAUNCH.md'];
  for (const [directory, version] of [[old,'old'],[next,'new']]) {
    await mkdir(join(directory, 'scripts'), {recursive:true});
    for (const name of names) await writeFile(join(directory,name),`${version} public ${name}`);
    await chmod(join(directory,'feishu-bridge'),0o700);
    const files=[...names];
    if (version==='new') { await mkdir(join(directory,'internal')); await writeFile(join(directory,'internal/new.go'),'new source'); files.push('internal/new.go'); }
    await writeManifest(directory,files);
  }
  await installPublicRelease(old,target);
  await writeFile(join(target,'config.json'),'private fixture must survive');
  await mkdir(join(target,'state'));
  await writeFile(join(target,'state/session.json'),'private session fixture');
  return {base,old,next,target,backup:join(base,'backup'),staging:join(base,'staging')};
}

test('public update and rollback preserve unknown private fixtures and restore manifest', async()=>{
  const f=await fixture();
  const expected=await preparePublicUpdate(f.next,f.target,f.backup,f.staging);
  assert.equal((await verifyRelease(f.target)).size,7);
  await applyPublicUpdate(f.staging,f.backup,f.target,expected);
  assert.equal((await verifyRelease(f.target)).size,8);
  assert.equal(await readFile(join(f.target,'feishu-bridge'),'utf8'),'new public feishu-bridge');
  await applyPublicUpdate(f.staging,f.backup,f.target,expected,{rollback:true});
  assert.equal((await verifyRelease(f.target)).size,7);
  assert.equal(await readFile(join(f.target,'config.json'),'utf8'),'private fixture must survive');
  assert.equal(await readFile(join(f.target,'state/session.json'),'utf8'),'private session fixture');
  assert.equal(await readFile(join(f.target,'feishu-bridge'),'utf8'),'old public feishu-bridge');
});

test('partial public update can roll back even before the manifest changes', async()=>{
  const f=await fixture(); const expected=await preparePublicUpdate(f.next,f.target,f.backup,f.staging);
  await assert.rejects(applyPublicUpdate(f.staging,f.backup,f.target,expected,{afterFile:async name=>{if(name==='feishu-bridge') throw new Error('fixture IO failure');}}),/fixture IO failure/);
  await applyPublicUpdate(f.staging,f.backup,f.target,expected,{rollback:true});
  assert.equal((await verifyRelease(f.target)).size,7);
  assert.equal(await readFile(join(f.target,'feishu-bridge'),'utf8'),'old public feishu-bridge');
});

async function addPublicFile(directory, name) {
  const files = [...(await verifyRelease(directory)).keys(), name];
  await mkdir(join(directory, name, '..'), {recursive:true});
  await writeFile(join(directory, name), 'old manifest-owned development record');
  await unlink(join(directory,'MANIFEST.sha256'));
  await writeManifest(directory, files);
}

test('retired manifest-owned development records are removed and restored by rollback', async()=>{
  const f=await fixture();
  await addPublicFile(f.target,'docs/HANDOFF.md');
  await addPublicFile(f.target,'AGENTS.md');
  await writeFile(join(f.target,'docs/PLAN.md'),'unknown local record');
  const expected=await preparePublicUpdate(f.next,f.target,f.backup,f.staging);
  await applyPublicUpdate(f.staging,f.backup,f.target,expected);
  for(const name of ['docs/HANDOFF.md','AGENTS.md']) {
    await assert.rejects(readFile(join(f.target,name)),{code:'ENOENT'});
  }
  assert.equal(await readFile(join(f.target,'docs/PLAN.md'),'utf8'),'unknown local record');
  assert.equal((await verifyRelease(f.target)).size,8);
  await applyPublicUpdate(f.staging,f.backup,f.target,expected,{rollback:true});
  assert.equal((await verifyRelease(f.target)).size,9);
  assert.equal(await readFile(join(f.target,'docs/HANDOFF.md'),'utf8'),'old manifest-owned development record');
  assert.equal(await readFile(join(f.target,'AGENTS.md'),'utf8'),'old manifest-owned development record');
  assert.equal(await readFile(join(f.target,'config.json'),'utf8'),'private fixture must survive');
});

test('failure after retirement restores missing public records before manifest publication', async()=>{
  const f=await fixture();
  await addPublicFile(f.target,'docs/HANDOFF.md');
  const expected=await preparePublicUpdate(f.next,f.target,f.backup,f.staging);
  await assert.rejects(applyPublicUpdate(f.staging,f.backup,f.target,expected,{
    afterFile:async name=>{if(name==='docs/HANDOFF.md') throw new Error('retirement IO failure');},
  }),/retirement IO failure/);
  await applyPublicUpdate(f.staging,f.backup,f.target,expected,{rollback:true});
  assert.equal((await verifyRelease(f.target)).size,8);
  assert.equal(await readFile(join(f.target,'docs/HANDOFF.md'),'utf8'),'old manifest-owned development record');
});

test('unreviewed removals and changed retired records fail before public replacement', async()=>{
  const f=await fixture();
  await addPublicFile(f.target,'internal/keep.go');
  await assert.rejects(preparePublicUpdate(f.next,f.target,f.backup,f.staging),{code:'public_manifest_removal_rejected'});
  assert.equal(await readFile(join(f.target,'feishu-bridge'),'utf8'),'old public feishu-bridge');
  const g=await fixture();
  await addPublicFile(g.target,'docs/HANDOFF.md');
  const expected=await preparePublicUpdate(g.next,g.target,g.backup,g.staging);
  await writeFile(join(g.target,'docs/HANDOFF.md'),'changed after preparation');
  await assert.rejects(applyPublicUpdate(g.staging,g.backup,g.target,expected),/checksum mismatch|changed/);
  assert.equal(await readFile(join(g.target,'feishu-bridge'),'utf8'),'old public feishu-bridge');
});

test('new public paths cannot overwrite unknown colliding files or follow symlinks',async()=>{
  for(const kind of ['collision','symlink']) {
    const f=await fixture();
    if(kind==='collision'){await mkdir(join(f.target,'internal'));await writeFile(join(f.target,'internal/new.go'),'unknown file');}
    else await symlink(join(f.next,'internal'),join(f.target,'internal'));
    await assert.rejects(preparePublicUpdate(f.next,f.target,f.backup,f.staging),/collision|symlink/);
    assert.equal(await readFile(join(f.target,'feishu-bridge'),'utf8'),'old public feishu-bridge');
  }
});

test('changed installation or staging cancels before public replacement',async()=>{
  for(const where of ['target','staging']) {
    const f=await fixture(); const expected=await preparePublicUpdate(f.next,f.target,f.backup,f.staging);
    await writeFile(join(f[where],'README.md'),'unexpected change');
    await assert.rejects(applyPublicUpdate(f.staging,f.backup,f.target,expected),/checksum mismatch|changed/);
    assert.equal(await readFile(join(f.target,'feishu-bridge'),'utf8'),'old public feishu-bridge');
  }
});

test('snapshot parent symlinks are rejected before following them',async()=>{
  const f=await fixture();
  await symlink(f.next,join(f.base,'snapshot-parent'));
  await assert.rejects(preparePublicUpdate(f.next,f.target,f.backup,join(f.base,'snapshot-parent/staging')),/snapshot parent symlink/);
  assert.equal(await readFile(join(f.target,'feishu-bridge'),'utf8'),'old public feishu-bridge');
});
