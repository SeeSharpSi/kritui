'use strict';

const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { execFile } = require('node:child_process');
const { promisify } = require('node:util');

const execFileAsync = promisify(execFile);

const workspaceRoot = path.resolve(__dirname, '..', '..');
const tempRoot = process.env.KRITUI_PLAYWRIGHT_TMPDIR ||
  path.join(os.homedir(), '.cache', 'opencode', 'tmp', 'opencode');

function uniqueBaseDir() {
  return path.join(tempRoot, `kritui-playwright-${Date.now()}-${process.pid}`);
}

function shouldCopy(source) {
  const relative = path.relative(workspaceRoot, source);
  if (!relative) {
    return true;
  }
  const parts = relative.split(path.sep);
  const first = parts[0];
  if (['.git', 'playwright', 'node_modules', 'test-results', 'tmp'].includes(first)) {
    return false;
  }
  if (first.startsWith('.env') || first.startsWith('data.db')) {
    return false;
  }
  const base = path.basename(relative);
  if (base === '.env' || base.startsWith('.env.') || base.startsWith('data.db') || base.endsWith('_test.go')) {
    return false;
  }
  if (base === '.air.toml' || base === 'opencode.json') {
    return false;
  }
  return true;
}

async function run(command, argumentsList, { cwd, env, timeout }) {
  try {
    const result = await execFileAsync(command, argumentsList, {
      cwd,
      env,
      timeout,
      maxBuffer: 64 * 1024 * 1024,
    });
    return result;
  } catch (error) {
    const stdout = error.stdout ? error.stdout.toString() : '';
    const stderr = error.stderr ? error.stderr.toString() : '';
    throw new Error(
      `${command} ${argumentsList.join(' ')} failed (${error.code ?? error.signal ?? 'unknown'}).\n` +
      `stdout:\n${tail(stdout)}\nstderr:\n${tail(stderr)}`,
    );
  }
}

function tail(value, lines = 80) {
  return value.split(/\r?\n/).slice(-lines).join('\n');
}

async function buildApp() {
  await fs.mkdir(tempRoot, { recursive: true });
  const base = uniqueBaseDir();
  const source = path.join(base, 'src');
  const binary = path.join(base, 'kritui');
  await fs.mkdir(source, { recursive: true });
  await fs.cp(workspaceRoot, source, { recursive: true, filter: shouldCopy });

  const env = {
    ...process.env,
    GOTOOLCHAIN: 'local',
    CGO_ENABLED: '0',
  };
  await run('go', ['run', 'github.com/a-h/templ/cmd/templ@v0.3.1020', 'generate'], {
    cwd: source,
    env,
    timeout: 180000,
  });
  await run('go', ['build', '-trimpath', '-o', binary, '.'], {
    cwd: source,
    env,
    timeout: 240000,
  });
  return { base, source, binary };
}

async function cleanupBuild(build) {
  if (build?.base) {
    await fs.rm(build.base, { recursive: true, force: true, maxRetries: 3 });
  }
}

module.exports = { buildApp, cleanupBuild, tail, tempRoot, workspaceRoot };
