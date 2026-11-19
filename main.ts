import { spawn } from 'node:child_process';
import { accessSync, chmodSync, constants, copyFileSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

process.env.UV_THREADPOOL_SIZE = '1';

const rootDir = dirname(fileURLToPath(import.meta.url));

function selectBinaryName(): string {
  if (process.platform === 'linux' && process.arch === 'x64') {
    return 'forge-linux-amd64';
  }
  if (process.platform === 'linux' && process.arch === 'arm64') {
    return 'forge-linux-arm64';
  }
  if (process.platform === 'darwin' && process.arch === 'arm64') {
    return 'forge-local';
  }
  return 'forge-linux-amd64';
}

function resolveExecutable(candidate: string): string {
  if (!existsSync(candidate)) {
    throw new Error(`compiled forge binary not found at ${candidate}`);
  }
  try {
    accessSync(candidate, constants.X_OK);
    return candidate;
  } catch {
    const tmpBin = '/tmp/gitforge-bin';
    copyFileSync(candidate, tmpBin);
    chmodSync(tmpBin, 0o755);
    return tmpBin;
  }
}

function launch(): void {
  const binPath = resolveExecutable(join(rootDir, selectBinaryName()));
  const extraArgs = process.argv.slice(2);
  let child = spawn(binPath, extraArgs, {
    stdio: ['ignore', 'inherit', 'inherit'],
    env: process.env,
  });

  const forwardSignal = (sig: NodeJS.Signals): void => {
    if (!child.killed) {
      child.kill(sig);
    }
  };
  process.on('SIGINT', () => forwardSignal('SIGINT'));
  process.on('SIGTERM', () => forwardSignal('SIGTERM'));

  child.on('error', (err: NodeJS.ErrnoException) => {
    // If /bot is mounted with noexec or lacks permissions, fall back to /tmp.
    if (err.code === 'EACCES' || err.code === 'EPERM' || err.code === 'EROFS') {
      try {
        const tmpBin = '/tmp/gitforge-bin';
        copyFileSync(join(rootDir, selectBinaryName()), tmpBin);
        chmodSync(tmpBin, 0o755);
        child = spawn(tmpBin, extraArgs, { stdio: ['ignore', 'inherit', 'inherit'], env: process.env });
        child.on('exit', (code, signal) => {
          process.exit(code ?? (signal ? 1 : 0));
        });
        return;
      } catch (copyErr) {
        process.stderr.write(`[launcher] fallback failed: ${String(copyErr)}\n`);
      }
    }
    process.stderr.write(`[launcher] failed to start ${binPath}: ${err.message}\n`);
    process.exit(1);
  });

  child.on('exit', (code, signal) => {
    if (signal) {
      process.exit(1);
    }
    process.exit(code ?? 0);
  });
}

launch();
