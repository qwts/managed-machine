// Credential-free readiness check against the installed DeepSeek dependency
// tree. Its version command never loads these lazy native dependencies.
import { createRequire } from 'node:module';
import { dirname, join, resolve } from 'node:path';
import { accessSync, constants } from 'node:fs';
import { tmpdir } from 'node:os';

try {
  if (!['darwin', 'linux'].includes(process.platform)) {
    throw new Error(`native helper check unsupported on ${process.platform}`);
  }
  const requireDsh = createRequire(join(process.argv[2], 'package.json'));
  const requireLocal = createRequire(requireDsh.resolve('@deepseek-ai/dsh-subprocess-local'));
  const koffi = requireLocal('koffi');
  const getpid = koffi.load(null).func('int getpid(void)');
  if (getpid() !== process.pid) throw new Error('koffi native call failed');
  const ptyEntry = requireLocal.resolve('node-pty');
  if (process.platform === 'darwin') {
    // A non-executable helper can abort inside node-pty's native addon before
    // JavaScript sees an error. Check the actual selected native directory first.
    const native = createRequire(ptyEntry)('./utils').loadNativeModule('pty');
    accessSync(resolve(dirname(ptyEntry), native.dir, 'spawn-helper'), constants.X_OK);
  }
  const pty = requireLocal('node-pty');
  const marker = 'managed-machine-deepseek-ok';
  const terminal = pty.spawn(process.execPath, ['-e', `process.stdout.write('${marker}')`], {
    name: 'xterm', cols: 80, rows: 24, cwd: tmpdir(),
    env: { PATH: process.env.PATH, TERM: 'xterm' },
  });
  let output = '';
  await new Promise((resolve, reject) => {
    const data = terminal.onData(chunk => { output += chunk; });
    const timeout = setTimeout(() => {
      try { terminal.kill(); } catch { /* The process may already have exited. */ }
      reject(new Error('node-pty process timed out after 8 seconds'));
    }, 8000);
    terminal.onExit(({ exitCode }) => {
      clearTimeout(timeout);
      data.dispose();
      if (exitCode !== 0 || !output.includes(marker)) {
        reject(new Error(`node-pty process failed (exit ${exitCode})`));
      } else {
        resolve();
      }
    });
  });
  console.log('DeepSeek native helpers ready (koffi and node-pty spawn)');
} catch (error) {
  console.error(`DeepSeek native helper check failed: ${error.message}`);
  process.exitCode = 1;
}
