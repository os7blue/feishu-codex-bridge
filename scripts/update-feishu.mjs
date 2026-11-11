// Delayed update of the existing independent installation. Public files only;
// configuration and state are loaded exclusively by the normal bridge CLI.
import { spawn } from 'node:child_process';
import { mkdir, open, readFile, readdir, readlink, rename, unlink, writeFile } from 'node:fs/promises';
import { dirname, join, resolve, isAbsolute } from 'node:path';
import { fileURLToPath } from 'node:url';
import { applyPublicUpdate, preparePublicUpdate } from './feishu-update.mjs';
import { sha256, verifyRelease } from './feishu-release.mjs';
import { activityReport, holdOfflineLock, performMaintenance, performLegacyBootstrap, validateLegacyBootstrap, reviewedLegacyDigest, restartTime, requireBridgeProcessCount } from './feishu-maintenance.mjs';

const script = fileURLToPath(import.meta.url);
const root = resolve(dirname(script), '..');
const args = process.argv.slice(2);
const option = name => { const index = args.indexOf(name); return index < 0 ? undefined : args[index+1]; };
const targetArgument = option('--target');
if (!targetArgument || !isAbsolute(targetArgument)) throw new Error('--target must identify an absolute installation directory');
const target = resolve(targetArgument);
const runtime = join(target, 'feishu-bridge');
const runner = join(target, 'scripts/project.mjs');
const configArgument = option('--config');
const config = configArgument ? resolve(configArgument) : undefined;
const evidence = join(root, '.cache/evidence');
const sleep = ms => new Promise(resolve=>setTimeout(resolve,ms));
const lock = join(target,'.public-update.lock');
const run = (executable,argv) => new Promise(resolve=>{
  const child=spawn(executable,argv,{cwd:root,stdio:'ignore'});
  child.once('error',()=>resolve(-1));child.once('exit',code=>resolve(code??-1));
});
async function locate(){
  const bridges=[];
  for(const entry of await readdir('/proc')) {
    if(!/^[0-9]+$/.test(entry)) continue;
    try {if(await readlink(`/proc/${entry}/exe`)===runtime) bridges.push(Number(entry));} catch {}
  }
  return bridges;
}
if(args.includes('--locate')) {console.log(JSON.stringify({bridges:await locate()}));process.exit(0);}
if (!config) throw new Error('--config must identify the existing private configuration; it is loaded only by the bridge CLI');
const worker=args.includes('--worker');
const offline=args.includes('--offline');
const initialLegacy=args.includes('--initial-legacy');
const legacyInterruptionApproved=args.includes('--accept-legacy-interruption');
if((initialLegacy&&offline)||initialLegacy!==legacyInterruptionApproved) throw new Error('initial legacy rollout requires standalone explicit interruption approval');
const delay=Number(option('--delay-seconds'));
const release=resolve(option('--release')??root);
if(!Number.isInteger(delay)||delay<60||delay>300||dirname(release)!==join(root,'.cache/artifacts')||!release.startsWith(join(root,'.cache/artifacts/feishu-release-'))) throw new Error('invalid fixed-directory update arguments');
await mkdir(evidence,{recursive:true,mode:0o700});
const journal=worker?resolve(option('--journal')??root):join(evidence,`feishu-public-update-${Date.now()}.json`);
if(dirname(journal)!==evidence) throw new Error('invalid update journal path');
const transaction=journal.slice(journal.lastIndexOf('/')+1,-5);
let record=worker?JSON.parse(await readFile(journal,'utf8')):{
  phase:'preflight',prepared_at:new Date().toISOString(),click_demo_requested:args.includes('--click-demo'),offline,initial_legacy:initialLegacy,legacy_interruption_approved:legacyInterruptionApproved,target,
  staging:join(target,'.public-updates',transaction),backup:journal+'.previous-release',ready_file:journal+'.ready.json',interaction_file:journal+'.interaction.json',
};
if(record.staging!==join(target,'.public-updates',transaction)||record.backup!==journal+'.previous-release'||record.ready_file!==journal+'.ready.json') throw new Error('invalid update snapshot paths');
if(record.interaction_file!==journal+'.interaction.json') throw new Error('invalid interaction receipt path');
if(record.target!==target) throw new Error('maintenance target changed');
if(Boolean(record.offline)!==offline) throw new Error('offline maintenance mode changed');
if(Boolean(record.initial_legacy)!==initialLegacy||Boolean(record.legacy_interruption_approved)!==legacyInterruptionApproved) throw new Error('legacy rollout approval changed');
let stateGuard;
async function acquireStateGuard(){
  if(!stateGuard) stateGuard=await holdOfflineLock(process.execPath,[join(record.staging,'scripts/project.mjs'),
    join(record.staging,'feishu-bridge'),'--config',config,'--maintenance-hold'],{cwd:root});
  stateGuard.assertHeld();
  if((await locate()).length!==0) throw new Error('running bridge prevents offline file replacement');
  stateGuard.assertHeld();
}
async function releaseStateGuard(){
  if(!stateGuard) return;
  const guard=stateGuard;stateGuard=undefined;
  await guard.release();
}
async function replacePublicFiles(rollback=false){
  await acquireStateGuard();
  await applyPublicUpdate(record.staging,record.backup,target,record,{
    rollback,afterFile:()=>stateGuard.assertHeld(),
  });
  stateGuard.assertHeld();
}
async function save(phase,extra={}){
  record={...record,...extra,phase,updated_at:new Date().toISOString()};
  await writeFile(journal+'.tmp',JSON.stringify(record,null,2)+'\n',{mode:0o600});await rename(journal+'.tmp',journal);
}
async function identity(pid){
  if(!Number.isSafeInteger(pid)||pid<2||await readlink(`/proc/${pid}/exe`)!==runtime) throw new Error('bridge process identity mismatch');
  const stat=await readFile(`/proc/${pid}/stat`,'utf8');return stat.slice(stat.lastIndexOf(')')+1).trim().split(/\s+/)[19];
}
async function activity(mode,pid){
  // The candidate CLI loads configuration internally, but reads no saved
  // sessions here and starts no App Server/model. Only the live process can
  // decide that it is safe to stop.
  const result=await new Promise(resolve=>{
    const child=spawn(process.execPath,[join(record.staging,'scripts/project.mjs'),join(record.staging,'feishu-bridge'),
      '--config',config,'--maintenance',mode,'--maintenance-pid',String(pid)],{cwd:root,stdio:['ignore','pipe','ignore']});
    let text='',oversize=false;
    const timer=setTimeout(()=>{child.kill('SIGTERM');},12000);
    child.stdout.on('data',chunk=>{if(text.length+chunk.length>1024) oversize=true;else text+=chunk;});
    child.once('error',()=>{clearTimeout(timer);resolve(null);});
    child.once('exit',code=>{clearTimeout(timer);try{resolve(code===0&&!oversize?JSON.parse(text):null);}catch{resolve(null);}});
  });
  return activityReport(result,pid);
}
async function stop(pid,stamp){
  if(await identity(pid)!==stamp) throw new Error('process start time changed');
  const decision=await activity('stop',pid);
  if(decision.phase==='busy') return decision;
  // A lost control response may follow a committed shutdown. Poll the original
  // process even then; never report it preserved or start a duplicate on a guess.
  for(let i=0;i<80;i++) {
    try {
      if(await identity(pid)!==stamp) throw new Error('process identity changed during stop');
    } catch(error) {if(error.code==='ENOENT'||error.code==='ESRCH') return {phase:'stopped'};throw error;}
    await sleep(250);
  }
  throw new Error('graceful stop not confirmed');
}
async function stopLegacy(proof){
  validateLegacyBootstrap(proof);
  if(await identity(proof.pid)!==proof.processStart || sha256(await readFile(`/proc/${proof.pid}/exe`))!==reviewedLegacyDigest) throw new Error('reviewed legacy process changed');
  // The reviewed Go main handles SIGTERM by cancelling its root context and
  // closing the RPC/connection normally. Never escalate to SIGKILL.
  process.kill(proof.pid,'SIGTERM');
  for(let i=0;i<80;i++) {
    try { if(await identity(proof.pid)!==proof.processStart) throw new Error('legacy process identity changed'); }
    catch(error) { if(error.code==='ENOENT'||error.code==='ESRCH') return {phase:'stopped'}; throw error; }
    await sleep(250);
  }
  throw new Error('legacy graceful shutdown not confirmed');
}
async function start(expectReady){
  await releaseStateGuard();
  if((await locate()).length!==0) throw new Error('bridge already running; duplicate start refused');
  const argv=[runner,runtime,'--config',config];
  if(expectReady) argv.push('--ready-file',record.ready_file);
  if(expectReady) argv.push('--interaction-evidence-file',record.interaction_file);
  if(expectReady&&record.click_demo_requested===true) argv.push('--click-demo');
  const child=spawn(process.execPath,argv,{cwd:root,detached:true,stdio:'ignore'});
  let exited=false;child.once('error',()=>{exited=true;});child.once('exit',()=>{exited=true;});child.unref();
  for(let i=0;i<(expectReady?80:6);i++) {
    if(exited) break;
    if(expectReady) {
      try {
        const receipt=JSON.parse(await readFile(record.ready_file,'utf8'));
        if(receipt.ready===true&&Number.isSafeInteger(receipt.pid)&&await readlink(`/proc/${receipt.pid}/exe`)===runtime) return {status:'ready',pid:receipt.pid,stamp:await identity(receipt.pid)};
      } catch(error) {
        // The bridge creates the receipt exclusively, then writes JSON. A
        // concurrent read can briefly see an incomplete document.
        if(error.code!=='ENOENT'&&!(error instanceof SyntaxError)) break;
      }
    }
    await sleep(500);
  }
  const bridges=await locate();
  if(bridges.length===1) return {status:'unconfirmed',pid:bridges[0],stamp:await identity(bridges[0])};
  return {status:exited?'exited':'unknown'};
}
async function notify(text){
  const exitCode=await run(process.execPath,[runner,runtime,'--config',config,'--notify-owner',text]);
  await save(record.phase,{owner_notification_succeeded:exitCode===0});
}
let ownsLock=false;
try {
  if(!worker) {
    const bridges=await locate();
    requireBridgeProcessCount(bridges.length,offline);
    const pid=offline?null:bridges[0],stamp=offline?null:await identity(pid);
    if(initialLegacy) validateLegacyBootstrap({interruptionApproved:legacyInterruptionApproved,pid,processStart:stamp,binaryDigest:sha256(await readFile(`/proc/${pid}/exe`))});
    const handle=await open(lock,'wx',0o600);ownsLock=true;await handle.writeFile(journal);await handle.close();
    const expected=await preparePublicUpdate(release,target,record.backup,record.staging);
    const stagedRunner=join(record.staging,'scripts/project.mjs'),stagedBinary=join(record.staging,'feishu-bridge');
    if(await run(process.execPath,[stagedRunner,stagedBinary,'--config',config,'--check'])!==0) throw new Error('candidate preflight failed');
    let initialActivity;
    if(initialLegacy) {
      initialActivity={phase:'legacy_interruption_approved'};
    } else if(offline) {
      await acquireStateGuard();
      initialActivity={phase:'offline_locked'};
      await releaseStateGuard();
    } else {
      if(await identity(pid)!==stamp) throw new Error('bridge changed during preflight');
      initialActivity=await activity('status',pid);
    }
    if(!['idle','busy','offline_locked','legacy_interruption_approved'].includes(initialActivity.phase)) {
      const error=new Error('live activity control unavailable; legacy bridge requires separately reviewed initial rollout');
      error.code='activity_control_unavailable';throw error;
    }
    const files=(await verifyRelease(record.staging)).size;
    await save('scheduled',{pid,process_start:stamp,...expected,initial_activity:initialActivity.phase,activity_verified:!initialLegacy,scheduled_at:new Date().toISOString(),planned_restart_at:restartTime(delay)});
    const workerArgs=[script,'--worker','--release',release,'--delay-seconds',String(delay),'--journal',journal,'--target',target,'--config',config];
    if(offline) workerArgs.push('--offline');
    if(initialLegacy) workerArgs.push('--initial-legacy','--accept-legacy-interruption');
    const child=spawn(process.execPath,workerArgs,{cwd:root,detached:true,stdio:'ignore'});
    await new Promise((resolve,reject)=>{child.once('spawn',resolve);child.once('error',reject);});child.unref();
    ownsLock=false;
    console.log(JSON.stringify({phase:'scheduled',offline,initial_legacy:initialLegacy,activity_verified:!initialLegacy,journal,planned_restart_at:record.planned_restart_at,files,binary_sha256:record.binary_sha256}));
  } else {
    if(await readFile(lock,'utf8')!==journal) throw new Error('maintenance lock mismatch');ownsLock=true;
    await sleep(Math.max(0,Date.parse(record.planned_restart_at)-Date.now()));
    await verifyRelease(target);await verifyRelease(record.staging);await verifyRelease(record.backup);
    if(sha256(await readFile(join(target,'MANIFEST.sha256')))!==record.old_manifest_sha256||(!offline&&await identity(record.pid)!==record.process_start)) throw new Error('preflight identities changed');
    const lifecycle={
      save, checkActivity:async()=>{
        if(!offline) return activity('status',record.pid);
        await acquireStateGuard();return {phase:'idle'};
      },
      stopCurrent:async()=>{
        if(!offline) return stop(record.pid,record.process_start);
        await acquireStateGuard();return {phase:'stopped'};
      },
      stopLegacy,
      install:()=>replacePublicFiles(),
      rollback:()=>replacePublicFiles(true),
      start, running:locate, stopStarted:async next=>{if((await stop(next.pid,next.stamp)).phase!=='stopped') throw new Error('replacement has unfinished work');},
      fingerprint:async()=>sha256(await readFile(runtime)), expectedDigest:record.binary_sha256,
      notify:phase=>notify(phase==='ready'
        ?'桥接程序已更新，飞书长连接已恢复。既有会话保留；可用 /help 查看本版用法，/commands 打开可修改、手动发送的命令草稿。实际客户端交互效果待你验证。'
        :phase==='deferred'
          ?'桥接更新已延期：切换前未能确认空闲或取得独占锁，没有替换公共程序、没有确认停止原桥接。请检查原进程状态后重新安排维护。'
        :phase==='rolled_back'
          ?'新版桥接未完成就绪核验，已恢复旧版公共程序。旧版连接状态待实聊确认；会话和配置未修改。'
          :'桥接维护未按计划完成，请核对维护记录；暂不报告更新成功。'),
    };
    const phase=initialLegacy
      ?await performLegacyBootstrap(lifecycle,{interruptionApproved:legacyInterruptionApproved,pid:record.pid,processStart:record.process_start,binaryDigest:sha256(await readFile(`/proc/${record.pid}/exe`))})
      :await performMaintenance(lifecycle);
    if(phase!=='ready') process.exitCode=1;
  }
} catch(error) {
  await save('failed',{reason:typeof error.code==='string'?error.code:'maintenance_failed'});
  if(worker) {try {await notify('桥接维护未按计划完成，请核对维护记录；暂不报告更新成功。');} catch {}}
  else console.error(JSON.stringify({phase:'failed',reason:record.reason,bridge_stop_requested:false,journal}));
  process.exitCode=1;
} finally {
  try {await releaseStateGuard();} catch {process.exitCode=1;}
  if(ownsLock) {try {await unlink(lock);} catch {}}
}
