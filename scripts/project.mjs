// Standalone bridge runner: caches belong to this deployment. No Guard business
// environment or private bridge files are read or changed here.
import { spawn } from 'node:child_process';
import { mkdir, chmod } from 'node:fs/promises';
import { accessSync, constants } from 'node:fs';
import { dirname, resolve, delimiter } from 'node:path';
import { homedir } from 'node:os';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const cache = resolve(root, '.cache');
await mkdir(cache, { recursive: true, mode: 0o700 });
await chmod(cache, 0o700);
for (const name of ['tmp', 'node', 'go/path', 'go/build', 'go/mod', 'go/tmp', 'xdg/cache', 'xdg/config', 'xdg/data', 'xdg/state']) {
  await mkdir(resolve(cache, name), { recursive: true, mode: 0o700 });
}
const env = { ...process.env,
  TMPDIR: resolve(cache, 'tmp'), TMP: resolve(cache, 'tmp'), TEMP: resolve(cache, 'tmp'),
  NODE_COMPILE_CACHE: resolve(cache, 'node'), PYTHONDONTWRITEBYTECODE: '1',
  GOPATH: resolve(cache, 'go/path'), GOCACHE: resolve(cache, 'go/build'),
  GOMODCACHE: resolve(cache, 'go/mod'), GOTMPDIR: resolve(cache, 'go/tmp'),
  GOTOOLCHAIN: 'local', GOTELEMETRY: 'off',
  XDG_CACHE_HOME: resolve(cache, 'xdg/cache'), XDG_CONFIG_HOME: resolve(cache, 'xdg/config'),
  XDG_DATA_HOME: resolve(cache, 'xdg/data'), XDG_STATE_HOME: resolve(cache, 'xdg/state'),
};
delete env.GUARD_CLIENT_DATA;
delete env.GUARD_DATA_DIR;
const [command, ...args] = process.argv.slice(2);
if (!command) throw new Error('Usage: node scripts/project.mjs COMMAND [ARGS...]');
if (command === 'go' || command === 'gofmt') {
  const available = path => { try { accessSync(path, constants.X_OK); return true; } catch { return false; } };
  if (!(env.PATH ?? '').split(delimiter).some(path => available(resolve(path || root, command)))) {
    const bin = [process.env.VFOX_HOME, resolve(homedir(), '.vfox')].filter(Boolean)
      .map(home => resolve(home, 'sdks/golang/bin')).find(path => available(resolve(path, command)));
    if (bin) env.PATH = [bin, env.PATH].filter(Boolean).join(delimiter);
  }
}
const child = spawn(command, args, { cwd: root, env, stdio: 'inherit', shell: false });
child.on('error', error => { console.error(`Unable to start requested tool: ${error.code ?? 'spawn_failed'}`); process.exitCode = 1; });
child.on('exit', (code, signal) => { process.exitCode = code ?? (signal === 'SIGINT' ? 130 : signal === 'SIGTERM' ? 143 : 1); });
