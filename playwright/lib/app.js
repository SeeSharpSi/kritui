'use strict';

const { spawn } = require('node:child_process');
const fs = require('node:fs/promises');
const net = require('node:net');
const path = require('node:path');

const { sleep, waitUntil } = require('./wait');

const BASE_URL = 'http://127.0.0.1:8080';
const APP_PORT = 8080;

function canConnect(host, port, timeout = 400) {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host, port });
    let settled = false;
    const finish = (value) => {
      if (settled) {
        return;
      }
      settled = true;
      socket.destroy();
      resolve(value);
    };
    socket.once('connect', () => finish(true));
    socket.once('error', () => finish(false));
    socket.setTimeout(timeout, () => finish(false));
  });
}

async function portInUse(port = APP_PORT) {
  const [ipv4, ipv6] = await Promise.all([
    canConnect('127.0.0.1', port),
    canConnect('::1', port),
  ]);
  return ipv4 || ipv6;
}

async function ensurePortFree() {
  if (await portInUse()) {
    throw new Error(
      `Port ${APP_PORT} is already in use. Refusing to start or stop any unrelated service. ` +
      'Stop the process using :8080 and rerun the Playwright suite.',
    );
  }
}

async function waitForPortFree() {
  await waitUntil(async () => !(await portInUse()), {
    timeout: 10000,
    label: `port ${APP_PORT} to become free`,
  });
}

class AppProcess {
  constructor({ child, cwd, logPath, logStream }) {
    this.child = child;
    this.cwd = cwd;
    this.logPath = logPath;
    this.logStream = logStream;
    this.logs = '';
    this.exit = new Promise((resolve) => {
      child.once('exit', (code, signal) => resolve({ code, signal }));
      child.once('error', (error) => resolve({ code: null, signal: null, error }));
    });
  }

  static async start({ binary, cwd, provider, logPath }) {
    await ensurePortFree();
    await fs.mkdir(cwd, { recursive: true });
    const stream = await fs.open(logPath, 'a');
    const child = spawn(binary, [], {
      cwd,
      env: {
        PATH: process.env.PATH ?? '/usr/bin:/bin',
        HOME: process.env.HOME ?? '',
        LANG: 'C.UTF-8',
        TZ: 'UTC',
        KRITUI_SECURE_COOKIES: 'false',
        LLM_KEY: 'test-key',
        LLM_MODEL: 'fake-model',
        LLM_ENDPOINT: provider.chatURL,
        SHOW_TOOLCALLS: 'false',
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    const app = new AppProcess({ child, cwd, logPath, logStream: stream });
    child.stdout.on('data', (chunk) => app._record(chunk));
    child.stderr.on('data', (chunk) => app._record(chunk));
    await app.waitForHealth();
    return app;
  }

  _record(chunk) {
    const text = chunk.toString('utf8');
    this.logs = (this.logs + text).slice(-200000);
    void this.logStream.write(text).catch(() => {});
  }

  get pid() {
    return this.child.pid;
  }

  async waitForHealth(timeout = 20000) {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      if (this.child.exitCode !== null) {
        throw new Error(`Kritui exited before becoming healthy.\napp log:\n${tail(this.logs)}`);
      }
      try {
        const response = await fetch(`${BASE_URL}/healthz`, {
          signal: AbortSignal.timeout(1000),
          redirect: 'manual',
        });
        if (response.ok) {
          return;
        }
      } catch {
        // The server is still starting, or the port is not accepting yet.
      }
      await sleep(50);
    }
    throw new Error(`Kritui did not become healthy within ${timeout}ms.\napp log:\n${tail(this.logs)}`);
  }

  async stop(signal = 'SIGTERM') {
    if (!this.child || this.child.exitCode !== null) {
      await this._closeStream();
      return;
    }
    this.child.kill(signal);
    let exited = await Promise.race([
      this.exit.then(() => true),
      sleep(5000).then(() => false),
    ]);
    if (!exited) {
      this.child.kill('SIGKILL');
      exited = await Promise.race([
        this.exit.then(() => true),
        sleep(2000).then(() => false),
      ]);
    }
    await this._closeStream();
    if (exited) {
      await waitForPortFree().catch(() => {});
    }
  }

  async _closeStream() {
    if (!this.logStream) {
      return;
    }
    const stream = this.logStream;
    this.logStream = null;
    await stream.close().catch(() => {});
  }
}

function tail(value, lines = 100) {
  return String(value ?? '').split(/\r?\n/).slice(-lines).join('\n');
}

class TestEnvironment {
  constructor({ build, provider, runDir }) {
    this.build = build;
    this.provider = provider;
    this.runDir = runDir;
    this.app = null;
    this.page = null;
    this.context = null;
  }

  async startApp() {
    this.app = await AppProcess.start({
      binary: this.build.binary,
      cwd: this.runDir,
      provider: this.provider,
      logPath: path.join(this.runDir, 'app.log'),
    });
    return this.app;
  }

  async restartApp({ signal = 'SIGKILL' } = {}) {
    if (this.app) {
      await this.app.stop(signal);
      this.app = null;
    }
    await waitForPortFree();
    return this.startApp();
  }

  get baseURL() {
    return BASE_URL;
  }

  appLogs() {
    return this.app?.logs ?? '';
  }
}

module.exports = {
  APP_PORT,
  AppProcess,
  BASE_URL,
  TestEnvironment,
  ensurePortFree,
  portInUse,
  waitForPortFree,
};
